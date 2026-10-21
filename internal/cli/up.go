package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/graph"
	"github.com/hjames9/kraze/internal/history"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"github.com/hjames9/kraze/internal/ui"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	upWait    bool
	upTimeout string
	upNoWait  bool
	upNoDeps  bool
	upLabels  []string

	upRemoveOrphans bool
)

var upCmd = &cobra.Command{
	Use:   "up [services...]",
	Short: "Install and start services",
	Long: `Install and start one or more services defined in kraze.yml.

If no services are specified, all services will be installed.
Services will be installed in dependency order automatically.

You can filter services by name or by labels:
  kraze up service1 service2      # Install specific services (with dependencies)
  kraze up service1 --no-deps     # Install service1 only, skip dependencies
  kraze up --label env=dev        # Install services with label env=dev
  kraze up --label tier=backend   # Install services with label tier=backend

Services this project installed earlier that are no longer in its config
(orphans) are left running. --remove-orphans removes them after a full up:
  kraze up --remove-orphans`,
	ValidArgsFunction: getServiceNames,
	RunE:              runUp,
}

func runUp(cmd *cobra.Command, args []string) (retErr error) {
	ctx := cmd.Context()

	loaded, cleanupPack, err := loadConfig(cmd, "Starting services from config file(s): %s")
	if err != nil {
		return err
	}
	defer cleanupPack()
	cfg := loaded.cfg

	if err := requireDocker(ctx, cfg); err != nil {
		return err
	}

	// Filter services if specified (including dependencies)
	requestedServices := args

	// Check if both service names and labels are specified
	if len(requestedServices) > 0 && len(upLabels) > 0 {
		return fmt.Errorf("cannot specify both service names and labels, use one or the other")
	}

	// Orphans are services not in the config, so only a full up can tell
	if upRemoveOrphans && (len(requestedServices) > 0 || len(upLabels) > 0) {
		return fmt.Errorf("--remove-orphans can't be combined with service names or --label")
	}
	configured := configuredServices(cfg)
	configuredNS := configuredNamespaces(cfg)

	// Validate --no-deps flag usage
	if upNoDeps {
		if len(upLabels) > 0 {
			return fmt.Errorf("--no-deps cannot be used with --label")
		}
		if len(requestedServices) == 0 {
			return fmt.Errorf("--no-deps requires specific service names to be provided")
		}
	}

	if len(upLabels) > 0 {
		// Filter by labels
		Verbose("Filtering services by labels: %v", upLabels)
		filteredServices, err := cfg.FilterServicesByLabelsWithDependencies(upLabels)
		if err != nil {
			return fmt.Errorf("failed to filter services by labels: %w", err)
		}
		cfg.Services = filteredServices
		Verbose("Found %d service(s) matching labels (including dependencies)", len(filteredServices))
	} else if len(requestedServices) > 0 {
		// Filter by service names
		Verbose("Services to install: %v", requestedServices)
		var filteredServices map[string]config.ServiceConfig
		var err error

		if upNoDeps {
			// Skip dependencies - install only requested services
			Verbose("--no-deps flag set, skipping dependencies")
			filteredServices, err = cfg.FilterServicesNoDependencies(requestedServices)
			if err != nil {
				return fmt.Errorf("failed to filter services: %w", err)
			}

			// Clear dependency references since we're intentionally ignoring them
			for name, svc := range filteredServices {
				svc.DependsOn = nil
				filteredServices[name] = svc
			}

			Verbose("Found %d service(s) (dependencies excluded)", len(filteredServices))
		} else {
			// Include dependencies (default behavior)
			filteredServices, err = cfg.FilterServicesWithDependencies(requestedServices)
			if err != nil {
				return fmt.Errorf("failed to filter services: %w", err)
			}
		}

		cfg.Services = filteredServices
	} else {
		Verbose("No services specified, will install all services")
	}

	// Filter out disabled services
	disabledCount := 0
	enabledServices := make(map[string]config.ServiceConfig)
	for name, svc := range cfg.Services {
		if svc.IsEnabled() {
			enabledServices[name] = svc
		} else {
			disabledCount++
			Verbose("Skipping disabled service '%s'", name)
		}
	}
	if disabledCount > 0 {
		Verbose("Filtered out %d disabled service(s)", disabledCount)
	}
	cfg.Services = enabledServices

	if dryRun {
		fmt.Printf("[DRY RUN] Would install %d service(s)\n", len(cfg.Services))
		for name := range cfg.Services {
			fmt.Printf("  - %s\n", name)
		}
		return nil
	}

	// Create dependency graph
	depGraph := graph.NewDependencyGraph(cfg.Services)

	// Validate dependencies (skip if --no-deps is used since we're intentionally ignoring them)
	if !upNoDeps {
		if err := depGraph.Validate(); err != nil {
			return fmt.Errorf("dependency validation failed: %w", err)
		}
	} else {
		Verbose("Skipping dependency validation (--no-deps flag set)")
	}

	// Get installation order grouped by dependency level (for parallel installation)
	serviceLevels, err := depGraph.TopologicalSortByLevel()
	if err != nil {
		return fmt.Errorf("failed to resolve dependencies: %w", err)
	}

	// Flatten for progress tracking
	var orderedServices []*config.ServiceConfig
	for _, level := range serviceLevels {
		orderedServices = append(orderedServices, level...)
	}

	// Create progress manager
	progress := ui.NewProgressManager(verbose, plain, len(orderedServices))

	// Create or verify cluster. The same manager must create the cluster and
	// get its kubeconfig, since it remembers the network it used.
	kindMgr := deps.newClusterManager()
	conn, err := connectCluster(ctx, kindMgr, cfg, createMissing)
	if err != nil {
		return err
	}
	kubeconfig, clientset, clusterCreated := conn.kubeconfig, conn.clientset, conn.created

	rec := beginHistory(ctx, clientset, cfg.Project, "up", clusterCreated)
	defer func() { finishHistory(rec, retErr) }()

	// Load or create cluster state
	st, existing, err := loadClusterState(ctx, clientset, cfg)
	if err != nil {
		return err
	}
	if existing && !cfg.Cluster.IsExternal() {
		// GPU config mismatch check (GPU requires cluster recreation)
		if err := checkGPUCompatibility(st, cfg); err != nil {
			return err
		}
	}
	adoptProject(st, cfg, configured, true)
	if err := checkServiceConflicts(st, cfg.Services, cfg.Project, loaded.original); err != nil {
		return err
	}

	if clusterCreated {
		st.SetClusterConfig(clusterConfigHash(cfg.Cluster), cfg.Project)
	} else if !cfg.Cluster.IsExternal() {
		warnClusterConfigDrift(st, cfg)
	}

	// Store the original config paths (before pack extraction) so future commands
	// can locate the config or archive without -f.
	st.SetConfigPaths(loaded.original)
	if saveErr := st.Save(ctx, clientset); saveErr != nil {
		Verbose("Warning: failed to store config paths in cluster state: %v", saveErr)
	}

	// Orphans go before installs: a renamed service's objects (or a chart's
	// fixed-name ones) still belong to its old name, so installing the new name
	// first fails on ownership and the orphan would never be reached
	// A service that moved namespace or changed type would be installed
	// afresh, leaving its old installation behind and no longer tracked
	moved := movedInstalls(st, cfg.Services)
	if len(moved) > 0 && !upRemoveOrphans {
		return movedServicesError(moved, cfg.Services)
	}
	if upRemoveOrphans {
		backend := deps.orphanBackend(quietProviderOptions(cfg, kubeconfig))
		old := append(st.Orphans(configured), moved...)
		if err := removeOrphans(ctx, st, clientset, kubeconfig, old, configuredNS, backend, false, rec, Verbose); err != nil {
			return err
		}
	} else if len(requestedServices) == 0 && len(upLabels) == 0 {
		// Before installing: a renamed service's install fails until its old
		// name is removed, and this explains why
		printOrphanHint(st.Orphans(configured))
	}

	// Determine global wait behavior from CLI flags
	globalWait := upWait && !upNoWait
	globalTimeout := upTimeout
	if upNoWait {
		globalWait = false
	}

	// Create image manager for automatic image loading
	imgMgr := deps.newImageManager(verbose)

	defer progress.Stop()

	// Start progress display
	progress.Start(len(orderedServices), "Installing")

	// Initialize all services as pending
	for i, svc := range orderedServices {
		progress.UpdateService(i, svc.Name, ui.StatusPending, "")
	}

	successCount := 0
	serviceIndex := 0

	// Install services level by level (parallel within each level)
	for levelNum, level := range serviceLevels {
		progress.Verbose("Installing level %d with %d service(s)", levelNum, len(level))

		// Decide install vs upgrade before any goroutine starts writing to st
		actions := make(map[string]string, len(level))
		for _, svc := range level {
			actions[svc.Name] = history.ActionInstall
			if st.IsServiceInstalled(svc.Name) {
				actions[svc.Name] = history.ActionUpgrade
			}
		}

		if len(level) == 1 {
			// Single service - install sequentially without goroutine overhead
			svc := level[0]
			itr := serviceIndex

			start := time.Now()
			err := installService(ctx, svc, itr, cfg, kubeconfig, st, clientset, kindMgr, imgMgr, progress, globalWait, globalTimeout, verbose)
			rec.RecordChange(svc.Name, actions[svc.Name], time.Since(start), err)
			if err != nil {
				return fmt.Errorf("failed to install service '%s' in level %d: %w", svc.Name, levelNum, err)
			}
			successCount++
			serviceIndex++
		} else {
			// Multiple services - install in parallel
			// Service error tracking
			type serviceError struct {
				serviceName string
				err         error
			}

			var wg sync.WaitGroup
			errChan := make(chan serviceError, len(level))
			successChan := make(chan bool, len(level))

			// Print "Installing" headers in order before launching goroutines so
			// [N/total] lines appear sequentially in scrolling output, not in
			// arbitrary goroutine-scheduling order.
			startIdx := serviceIndex
			for i, svc := range level {
				progress.UpdateService(startIdx+i, svc.Name, ui.StatusInstalling, fmt.Sprintf("(%s)", svc.Type))
			}

			for _, svc := range level {
				wg.Add(1)
				itr := serviceIndex
				serviceIndex++

				go func(service *config.ServiceConfig, idx int) {
					defer wg.Done()

					start := time.Now()
					err := installService(ctx, service, idx, cfg, kubeconfig, st, clientset, kindMgr, imgMgr, progress, globalWait, globalTimeout, verbose)
					rec.RecordChange(service.Name, actions[service.Name], time.Since(start), err)
					if err != nil {
						progress.Verbose("Service '%s' failed in level %d: %v", service.Name, levelNum, err)
						errChan <- serviceError{serviceName: service.Name, err: err}
					} else {
						successChan <- true
					}
				}(svc, itr)
			}

			// Wait for all services in this level to complete
			wg.Wait()
			close(errChan)
			close(successChan)

			// Check for errors (fail-fast with context)
			if len(errChan) > 0 {
				svcErr := <-errChan
				return fmt.Errorf("failed to install service '%s' in level %d: %w", svcErr.serviceName, levelNum, svcErr.err)
			}

			// Count successes
			successCount += len(successChan)
		}

		// Apply post-ready delay after each level
		// This helps ensure all services in the level are stable before starting the next level
		if levelNum < len(serviceLevels)-1 {
			delay := 3 * time.Second
			progress.Verbose("Level %d complete, waiting %v before next level...", levelNum, delay)
			deps.sleep(delay)
		}
	}

	// Finish progress display
	progress.Finish(successCount)

	fmt.Printf("\nTo check status: kraze status\n")
	fmt.Printf("To tear down:    kraze down\n")

	return nil
}

// Mutex for protecting shared resources during parallel installation
var (
	dockerMutex sync.Mutex
	stateMutex  sync.Mutex
)

// installService installs a single service - can be called from a goroutine
func installService(
	ctx context.Context,
	svc *config.ServiceConfig,
	serviceIndex int,
	cfg *config.Config,
	kubeconfig string,
	st *state.ClusterState,
	clientset kubernetes.Interface,
	kindMgr clusterManager,
	imgMgr imageManager,
	progress ui.ProgressManager,
	globalWait bool,
	globalTimeout string,
	verbose bool,
) error {
	// Update progress to show we're installing this service
	progress.UpdateService(serviceIndex, svc.Name, ui.StatusInstalling, fmt.Sprintf("(%s)", svc.Type))
	progress.Verbose("Installing '%s' (%s)...", svc.Name, svc.Type)

	// Determine wait behavior for this service (precedence: service config > CLI flag)
	serviceWait := globalWait
	if svc.Wait != nil {
		serviceWait = *svc.Wait
		progress.Verbose("Service '%s' has wait=%v configured", svc.Name, serviceWait)
	}

	// Determine timeout for this service (precedence: service config > CLI flag)
	serviceTimeout := globalTimeout
	if svc.WaitTimeout != "" {
		serviceTimeout = svc.WaitTimeout
		progress.Verbose("Service '%s' has wait_timeout=%s configured", svc.Name, serviceTimeout)
	}

	// Create provider options
	providerOpts := quietProviderOptions(cfg, kubeconfig)
	providerOpts.Wait = serviceWait
	providerOpts.Timeout = serviceTimeout

	// Create provider for this service
	provider, err := deps.newProvider(svc, providerOpts)
	if err != nil {
		return fmt.Errorf("failed to create provider for '%s': %w", svc.Name, err)
	}

	// Extract images from service configuration (read-only, no lock needed)
	serviceImages, err := imgMgr.GetImagesForService(ctx, svc, kubeconfig)
	if err != nil {
		progress.Verbose("Warning: failed to extract images for '%s': %v", svc.Name, err)
		serviceImages = []string{}
	}

	if len(serviceImages) > 0 {
		// Check if context was cancelled before expensive Docker operations
		if ctx.Err() != nil {
			progress.UpdateService(serviceIndex, svc.Name, ui.StatusFailed, "Cancelled")
			return ctx.Err()
		}

		// Lock for Docker operations (pull/load) to avoid race conditions
		dockerMutex.Lock()

		progress.Verbose("Detected %d image(s) for service '%s': %v", len(serviceImages), svc.Name, serviceImages)

		// Get image info and hashes for all detected images
		imageHashes := make(map[string]string)
		localImages := make([]string, 0)

		for _, img := range serviceImages {
			imgInfo, err := imgMgr.GetImageInfo(ctx, img)
			if err != nil {
				progress.Verbose("Warning: failed to get info for image '%s': %v", img, err)
				continue
			}

			// Store the hash
			if imgInfo.SHA256 != "" {
				imageHashes[img] = imgInfo.SHA256
			}

			// Collect images present in local daemon (built or pre-pulled)
			if imgInfo.InLocalDaemon {
				localImages = append(localImages, img)
			}
		}

		// Determine which images need to be loaded
		imagesToLoad := make([]string, 0)
		imagesToRemove := make([]string, 0) // Track images that need to be removed before reloading

		// Only process local images (built locally, no registry source).
		// Registry images (those with RepoDigests) are pulled directly by kind's containerd
		// and do not need to be loaded via the host Docker daemon.
		for _, img := range serviceImages {
			currentHash := imageHashes[img]
			imgInfo, _ := imgMgr.GetImageInfo(ctx, img)

			if imgInfo != nil && imgInfo.InLocalDaemon {
				// Image is present in the local Docker daemon (built locally or pre-pulled).
				// Load it into the kind cluster rather than relying on kind's containerd
				// to pull from the registry — the registry may be unreachable from inside
				// the kind node network.
				needsLoad := false
				needsRemove := false
				if cfg.Cluster.IsExternal() {
					// External cluster - use state file comparison
					needsLoad = st.HasImageHashChanged(svc.Name, img, currentHash)
					if needsLoad {
						progress.Verbose("Image '%s' changed (state file), but external cluster - skipping auto-load", img)
						needsLoad = false
					} else {
						progress.Verbose("Image '%s' unchanged (hash matches state), skipping", img)
					}
				} else {
					// Kind cluster - compare with actual cluster
					clusterHash, err := imgMgr.GetClusterImageHash(ctx, cfg.Cluster.Name, img)
					if err != nil {
						progress.Verbose("Warning: failed to get cluster image hash for '%s': %v", img, err)
						needsLoad = true
					} else if clusterHash == "" {
						progress.Verbose("Image '%s' not found in cluster, will load", img)
						needsLoad = true
					} else if clusterHash != currentHash {
						progress.Verbose("Image '%s' changed (cluster: %s, local: %s), will reload", img, clusterHash[:12], currentHash[:12])
						needsLoad = true
						needsRemove = true // Remove old image before loading new one
					} else {
						progress.Verbose("Image '%s' unchanged (hash matches cluster), skipping load", img)
					}
				}

				if needsLoad {
					imagesToLoad = append(imagesToLoad, img)
					if needsRemove {
						imagesToRemove = append(imagesToRemove, img)
					}
				}
			} else {
				// Image is not in the local Docker daemon — kind will pull it from the registry.
				progress.Verbose("Image '%s' not found in local daemon, kind will pull from registry", img)
			}
		}

		// Untag old images that have changed (must be done before loading)
		// This removes the tag reference but leaves the image data so running pods aren't affected
		if len(imagesToRemove) > 0 {
			progress.Verbose("Untagging %d outdated image(s) from cluster...", len(imagesToRemove))
			for _, img := range imagesToRemove {
				progress.Verbose("Untagging old version of image '%s'...", img)
				if err := kindMgr.UntagImage(ctx, cfg.Cluster.Name, img); err != nil {
					progress.Verbose("Warning: failed to untag old image '%s': %v", img, err)
					progress.Verbose("  This may cause the tag to point to the old image even after loading the new one")
				} else {
					progress.Verbose("%s Old tag removed for '%s' (image data kept for running pods)", color.Checkmark(), img)
				}
			}
		}

		// Load images that need to be loaded
		if len(imagesToLoad) > 0 {
			progress.UpdateService(serviceIndex, svc.Name, ui.StatusInstalling, fmt.Sprintf("Loading %d image(s)", len(imagesToLoad)))

			for _, img := range imagesToLoad {
				progress.Verbose("Loading image '%s'...", img)
				if err := kindMgr.LoadImage(ctx, cfg.Cluster.Name, img); err != nil {
					progress.Verbose("Warning: failed to load image '%s': %v", img, err)
				} else {
					progress.Verbose("%s Image '%s' loaded", color.Checkmark(), img)
				}
			}

			progress.Verbose("%s Images loaded successfully", color.Checkmark())
		} else if len(localImages) > 0 {
			progress.Verbose("All %d local image(s) already loaded (hashes match)", len(localImages))
		}

		dockerMutex.Unlock()

		// Restart any pods stuck in ImagePullBackOff for this service's images.
		// Using serviceImages (not just imagesToLoad) covers images that were already
		// in the cluster — stale stuck pods from previous runs won't be caught otherwise.
		restartImagePullBackOffPods(ctx, clientset, svc.GetNamespace(), serviceImages, progress)

		// Store image hashes in state for future comparisons
		if len(imageHashes) > 0 {
			defer func(serviceName string, hashes map[string]string) {
				stateMutex.Lock()
				defer stateMutex.Unlock()
				if st.SetImageHashes(serviceName, hashes) {
					if err := st.Save(ctx, clientset); err != nil {
						progress.Verbose("Warning: failed to save cluster state (image hashes): %v", err)
					}
				}
			}(svc.Name, imageHashes)
		}
	}

	// Create the namespace ourselves, labeled as kraze-managed, rather than
	// leaving it to the provider, so `kraze down` knows it may delete it. An
	// existing namespace that carries the label was also created by kraze.
	namespace := svc.GetNamespace()
	namespaceExists, namespaceManaged, err := providers.NamespaceInfo(ctx, clientset, namespace)
	if err != nil {
		progress.Verbose("Warning: %v", err)
	}
	createdNamespace := namespaceManaged
	if err == nil && !namespaceExists && svc.ShouldCreateNamespace() {
		created, err := providers.EnsureNamespace(ctx, clientset, namespace)
		if err != nil {
			progress.UpdateService(serviceIndex, svc.Name, ui.StatusFailed, err.Error())
			return fmt.Errorf("failed to install '%s': %w", svc.Name, err)
		}
		createdNamespace = created
	}

	// Check if context was cancelled before installing
	if ctx.Err() != nil {
		progress.UpdateService(serviceIndex, svc.Name, ui.StatusFailed, "Cancelled")
		return ctx.Err()
	}

	// Update status to show we're applying resources
	progress.UpdateService(serviceIndex, svc.Name, ui.StatusInstalling, "Applying resources")

	// Record what was applied, so the service can be removed once it is no
	// longer in the config (--remove-orphans)
	record := func() {
		var resources []state.ResourceRef
		if mp, ok := provider.(providers.ResourceRecorder); ok {
			var err error
			if resources, err = mp.ResourceRefs(svc); err != nil {
				progress.Verbose("Warning: failed to record resources of '%s': %v", svc.Name, err)
			}
		}

		// Update cluster state with namespace tracking (protected by mutex)
		stateMutex.Lock()
		defer stateMutex.Unlock()
		st.MarkServiceInstalledWithNamespace(svc.Name, namespace, createdNamespace)
		st.SetServiceSource(svc.Name, svc.Type, resources)
		if err := st.Save(ctx, clientset); err != nil {
			progress.Verbose("Warning: failed to save cluster state: %v", err)
		}
	}

	// Install the service
	if err := provider.Install(ctx, svc); err != nil {
		progress.UpdateService(serviceIndex, svc.Name, ui.StatusFailed, err.Error())
		// A failed install can still leave objects (or a failed Helm release)
		// behind. Record it, or it couldn't be removed once dropped from the
		// config. An ownership conflict applied nothing.
		if !errors.Is(err, providers.ErrOwnershipConflict) {
			if installed, checkErr := provider.IsInstalled(context.WithoutCancel(ctx), svc); checkErr == nil && installed {
				record()
			}
		}
		return fmt.Errorf("failed to install '%s': %w", svc.Name, err)
	}
	record()

	// Mark service as ready
	progress.UpdateService(serviceIndex, svc.Name, ui.StatusReady, "Deployed")

	// Apply post-ready delay (defaults to 3 seconds)
	// This helps with kube-proxy propagation and service endpoint readiness
	delay, err := svc.GetPostReadyDelay()
	if err != nil {
		progress.Verbose("Warning: %v, using default 3s delay", err)
		delay = 3 * time.Second
	}
	if delay > 0 {
		progress.Verbose("Waiting %v for service to stabilize...", delay)
		deps.sleep(delay)
	}

	return nil
}

// restartImagePullBackOffPods deletes pods in the given namespace that are stuck
// in ImagePullBackOff or ErrImagePull for one of the just-loaded images. Deleting
// them causes the ReplicaSet controller to recreate them immediately, bypassing
// the exponential backoff that would otherwise delay recovery for minutes.
func restartImagePullBackOffPods(ctx context.Context, clientset kubernetes.Interface, namespace string, loadedImages []string, progress ui.ProgressManager) {
	if len(loadedImages) == 0 {
		return
	}

	// Build a normalized set of loaded image refs for fast lookup
	type imageKey struct{ registry, repository, tag string }
	loaded := make(map[imageKey]bool, len(loadedImages))
	for _, img := range loadedImages {
		ref := cluster.ParseImageReference(img)
		loaded[imageKey{ref.Registry, ref.Repository, ref.Tag}] = true
	}

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		progress.Verbose("Warning: failed to list pods in '%s': %v", namespace, err)
		return
	}

	for i := range pods.Items {
		pod := &pods.Items[i]

		// Collect all container statuses (regular + init)
		statuses := append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...)
		for _, cs := range statuses {
			reason := ""
			if cs.State.Waiting != nil {
				reason = cs.State.Waiting.Reason
			}
			if reason != "ImagePullBackOff" && reason != "ErrImagePull" {
				continue
			}

			ref := cluster.ParseImageReference(cs.Image)
			if !loaded[imageKey{ref.Registry, ref.Repository, ref.Tag}] {
				continue
			}

			progress.Verbose("Deleting pod '%s' stuck in %s for image '%s'", pod.Name, reason, cs.Image)
			if err := clientset.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
				progress.Verbose("Warning: failed to delete pod '%s': %v", pod.Name, err)
			} else {
				progress.Verbose("%s Deleted stuck pod '%s' — ReplicaSet will recreate it", color.Checkmark(), pod.Name)
			}
			break // one delete per pod is enough
		}
	}
}

func init() {
	upCmd.Flags().BoolVar(&upWait, "wait", true, "Wait for services to be ready")
	upCmd.Flags().BoolVar(&upNoWait, "no-wait", false, "Don't wait for services to be ready")
	upCmd.Flags().StringVar(&upTimeout, "timeout", "10m", "Timeout for wait operations")
	upCmd.Flags().BoolVar(&upNoDeps, "no-deps", false, "Don't install dependencies (only install specified services)")
	upCmd.Flags().BoolVar(&upRemoveOrphans, "remove-orphans", false, "Remove services this project installed that are no longer in its config")
	upCmd.Flags().StringSliceVarP(&upLabels, "label", "l", []string{}, "Filter services by label (format: key=value, can be specified multiple times)")
}
