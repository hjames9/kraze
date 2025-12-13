package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/graph"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"github.com/hjames9/kraze/internal/ui"
	"github.com/spf13/cobra"
)

var (
	upWait    bool
	upTimeout string
	upNoWait  bool
	upNoDeps  bool
	upLabels  []string
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
  kraze up --label tier=backend   # Install services with label tier=backend`,
	ValidArgsFunction: getServiceNames,
	RunE:              runUp,
}

func runUp(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	Verbose("Starting services from config file: %s", configFile)

	// Parse configuration
	cfg, err := config.Parse(configFile)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	// Check Docker availability (only for kind clusters, not external)
	if !cfg.Cluster.IsExternal() {
		Verbose("Checking Docker availability...")
		if err := cluster.CheckDockerAvailable(ctx); err != nil {
			return err
		}
		Verbose("Docker is available")
	}

	// Filter services if specified (including dependencies)
	requestedServices := args

	// Check if both service names and labels are specified
	if len(requestedServices) > 0 && len(upLabels) > 0 {
		return fmt.Errorf("cannot specify both service names and labels, use one or the other")
	}

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
	progress := ui.NewProgressManager(verbose, plain)

	// Create or verify cluster
	kindMgr := cluster.NewKindManager()

	exists, err := kindMgr.ClusterExists(cfg.Cluster.Name)
	if err != nil {
		return fmt.Errorf("failed to check cluster: %w", err)
	}

	if !exists {
		fmt.Printf("Cluster '%s' does not exist, creating it...\n", cfg.Cluster.Name)
		if err := kindMgr.CreateCluster(ctx, &cfg.Cluster); err != nil {
			return fmt.Errorf("failed to create cluster: %w", err)
		}

		// Update ~/.kube/config with cluster access (Use container IP)
		Verbose("Updating kubeconfig...")
		if err := kindMgr.UpdateKubeconfigFile(cfg.Cluster.Name); err != nil {
			Verbose("Warning: failed to update kubeconfig: %v", err)
		} else {
			Verbose("Kubeconfig updated (context: kind-%s)", cfg.Cluster.Name)
		}
	} else {
		Verbose("Cluster '%s' already exists", cfg.Cluster.Name)
	}

	// Get kubeconfig for the cluster (will be patched with container IP)
	kubeconfig, err := kindMgr.GetKubeConfig(cfg.Cluster.Name, false)
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	// Get state file path (in same directory as config file)
	stateFilePath := state.GetStateFilePath(".")

	// Load or create state
	st, err := state.Load(stateFilePath)
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}
	if st == nil {
		st = state.New(cfg.Cluster.Name, cfg.Cluster.IsExternal())
	}

	// Determine global wait behavior from CLI flags
	globalWait := upWait && !upNoWait
	globalTimeout := upTimeout
	if upNoWait {
		globalWait = false
	}

	// Create image manager for automatic image loading
	imgMgr := cluster.NewImageManager(verbose)

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

		if len(level) == 1 {
			// Single service - install sequentially without goroutine overhead
			svc := level[0]
			itr := serviceIndex

			if err := installService(ctx, svc, itr, cfg, kubeconfig, st, stateFilePath, kindMgr, imgMgr, progress, globalWait, globalTimeout, verbose); err != nil {
				return fmt.Errorf("failed to install service '%s' in level %d: %w", svc.Name, levelNum, err)
			}
			successCount++
			serviceIndex++
		} else {
			// Multiple services - install in parallel with cancellation
			levelCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			// Service error tracking
			type serviceError struct {
				serviceName string
				err         error
			}

			var wg sync.WaitGroup
			errChan := make(chan serviceError, len(level))
			successChan := make(chan bool, len(level))
			var firstError sync.Once

			for _, svc := range level {
				wg.Add(1)
				itr := serviceIndex
				serviceIndex++

				go func(service *config.ServiceConfig, idx int) {
					defer wg.Done()

					if err := installService(levelCtx, service, idx, cfg, kubeconfig, st, stateFilePath, kindMgr, imgMgr, progress, globalWait, globalTimeout, verbose); err != nil {
						// Cancel context on first error to stop other installations
						firstError.Do(func() {
							progress.Verbose("Service '%s' failed, cancelling other installations in level %d", service.Name, levelNum)
							cancel()
						})
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
			time.Sleep(delay)
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
	st *state.State,
	stateFilePath string,
	kindMgr *cluster.KindManager,
	imgMgr *cluster.ImageManager,
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
	providerOpts := &providers.ProviderOptions{
		ClusterName: cfg.Cluster.Name,
		KubeConfig:  kubeconfig,
		Wait:        serviceWait,
		Timeout:     serviceTimeout,
		Verbose:     verbose,
		Quiet:       !verbose, // Suppress intermediate output unless verbose
	}

	// Create provider for this service
	provider, err := providers.NewProvider(svc, providerOpts)
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

			// Collect local images
			if imgInfo.IsLocal {
				localImages = append(localImages, img)
			}
		}

		// Determine which images need to be loaded
		imagesToLoad := make([]string, 0)
		imagesToPull := make([]string, 0)
		imagesToRemove := make([]string, 0) // Track images that need to be removed before reloading

		// Separate local images (already in Docker) from remote images (need to pull)
		for _, img := range serviceImages {
			currentHash := imageHashes[img]
			imgInfo, _ := imgMgr.GetImageInfo(ctx, img)

			if imgInfo != nil && imgInfo.IsLocal {
				// Image is local - check if it needs to be loaded into cluster
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
				// Image is not local - need to pull it first
				imagesToPull = append(imagesToPull, img)
			}
		}

		// Pull remote images first
		if len(imagesToPull) > 0 {
			progress.UpdateService(serviceIndex, svc.Name, ui.StatusInstalling, fmt.Sprintf("Pulling %d image(s)", len(imagesToPull)))
			for _, img := range imagesToPull {
				progress.Verbose("Pulling image '%s'...", img)
				if err := kindMgr.PullImage(ctx, img); err != nil {
					progress.Verbose("Warning: failed to pull image '%s': %v", img, err)
				} else {
					progress.Verbose("%s Image '%s' pulled", color.Checkmark(), img)
					imagesToLoad = append(imagesToLoad, img)
				}
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

		// Store image hashes in state for future comparisons
		if len(imageHashes) > 0 {
			defer func(serviceName string, hashes map[string]string) {
				stateMutex.Lock()
				defer stateMutex.Unlock()
				if svc, exists := st.Services[serviceName]; exists {
					svc.ImageHashes = hashes
					st.Services[serviceName] = svc
					st.Save(stateFilePath)
				}
			}(svc.Name, imageHashes)
		}
	}

	// Check if namespace exists before installing (to track if we'll create it)
	namespace := svc.GetNamespace()
	namespaceExists, err := providers.CheckNamespaceExists(ctx, kubeconfig, namespace)
	if err != nil {
		progress.Verbose("Warning: failed to check if namespace '%s' exists: %v", namespace, err)
		namespaceExists = false
	}

	// We will create the namespace if:
	// 1. It doesn't exist AND
	// 2. create_namespace is true (which is now the default)
	willCreateNamespace := !namespaceExists && svc.ShouldCreateNamespace()

	// Check if context was cancelled before installing
	if ctx.Err() != nil {
		progress.UpdateService(serviceIndex, svc.Name, ui.StatusFailed, "Cancelled")
		return ctx.Err()
	}

	// Update status to show we're applying resources
	progress.UpdateService(serviceIndex, svc.Name, ui.StatusInstalling, "Applying resources")

	// Install the service
	if err := provider.Install(ctx, svc); err != nil {
		progress.UpdateService(serviceIndex, svc.Name, ui.StatusFailed, err.Error())
		return fmt.Errorf("failed to install '%s': %w", svc.Name, err)
	}

	// Update state with namespace tracking (protected by mutex)
	stateMutex.Lock()
	st.MarkServiceInstalledWithNamespace(svc.Name, namespace, willCreateNamespace)
	if err := st.Save(stateFilePath); err != nil {
		progress.Verbose("Warning: failed to save state: %v", err)
	}
	stateMutex.Unlock()

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
		time.Sleep(delay)
	}

	return nil
}

func init() {
	upCmd.Flags().BoolVar(&upWait, "wait", true, "Wait for services to be ready")
	upCmd.Flags().BoolVar(&upNoWait, "no-wait", false, "Don't wait for services to be ready")
	upCmd.Flags().StringVar(&upTimeout, "timeout", "10m", "Timeout for wait operations")
	upCmd.Flags().BoolVar(&upNoDeps, "no-deps", false, "Don't install dependencies (only install specified services)")
	upCmd.Flags().StringSliceVarP(&upLabels, "label", "l", []string{}, "Filter services by label (format: key=value, can be specified multiple times)")
}
