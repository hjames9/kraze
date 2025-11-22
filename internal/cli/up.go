package cli

import (
	"context"
	"fmt"

	"github.com/hjames9/kraze/internal/cluster"
	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/graph"
	"github.com/hjames9/kraze/internal/providers"
	"github.com/hjames9/kraze/internal/state"
	"github.com/spf13/cobra"
)

var (
	upWait    bool
	upTimeout string
	upNoWait  bool
	upLabels  []string
)

var upCmd = &cobra.Command{
	Use:   "up [services...]",
	Short: "Install and start services",
	Long: `Install and start one or more services defined in kraze.yml.

If no services are specified, all services will be installed.
Services will be installed in dependency order automatically.

You can filter services by name or by labels:
  kraze up service1 service2      # Install specific services
  kraze up --label env=dev        # Install services with label env=dev
  kraze up --label tier=backend   # Install services with label tier=backend`,
	RunE: runUp,
}

func runUp(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	Verbose("Starting services from config file: %s", configFile)

	// Parse configuration
	cfg, err := config.Parse(configFile)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	// Filter services if specified (including dependencies)
	requestedServices := args

	// Check if both service names and labels are specified
	if len(requestedServices) > 0 && len(upLabels) > 0 {
		return fmt.Errorf("cannot specify both service names and labels, use one or the other")
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
		filteredServices, err := cfg.FilterServicesWithDependencies(requestedServices)
		if err != nil {
			return fmt.Errorf("failed to filter services: %w", err)
		}
		cfg.Services = filteredServices
	} else {
		Verbose("No services specified, will install all services")
	}

	if dryRun {
		fmt.Printf("[DRY RUN] Would install %d service(s)\n", len(cfg.Services))
		for name := range cfg.Services {
			fmt.Printf("  - %s\n", name)
		}
		return nil
	}

	// Create dependency graph
	depGraph := graph.NewDependencyGraph(cfg.Services)

	// Validate dependencies
	if err := depGraph.Validate(); err != nil {
		return fmt.Errorf("dependency validation failed: %w", err)
	}

	// Get installation order (topological sort)
	orderedServices, err := depGraph.TopologicalSort()
	if err != nil {
		return fmt.Errorf("failed to resolve dependencies: %w", err)
	}

	fmt.Printf("Installing %d service(s) in dependency order...\n", len(orderedServices))

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

	// Install each service in dependency order
	for itr, svc := range orderedServices {
		fmt.Printf("\n[%d/%d] Installing '%s' (%s)...\n", itr+1, len(orderedServices), svc.Name, svc.Type)

		// Determine wait behavior for this service (precedence: service config > CLI flag)
		serviceWait := globalWait
		if svc.Wait != nil {
			serviceWait = *svc.Wait
			Verbose("Service '%s' has wait=%v configured", svc.Name, serviceWait)
		}

		// Determine timeout for this service (precedence: service config > CLI flag)
		serviceTimeout := globalTimeout
		if svc.WaitTimeout != "" {
			serviceTimeout = svc.WaitTimeout
			Verbose("Service '%s' has wait_timeout=%s configured", svc.Name, serviceTimeout)
		}

		// Create provider options
		providerOpts := &providers.ProviderOptions{
			ClusterName: cfg.Cluster.Name,
			KubeConfig:  kubeconfig,
			Wait:        serviceWait,
			Timeout:     serviceTimeout,
			Verbose:     verbose,
		}

		// Create provider for this service
		provider, err := providers.NewProvider(svc, providerOpts)
		if err != nil {
			return fmt.Errorf("failed to create provider for '%s': %w", svc.Name, err)
		}

		// Check if already installed
		installed, err := provider.IsInstalled(ctx, svc)
		if err != nil {
			Verbose("Warning: failed to check if '%s' is installed: %v", svc.Name, err)
			installed = false
		}

		if installed {
			fmt.Printf("Service '%s' is already installed, skipping...\n", svc.Name)
			continue
		}

		// Extract images from service configuration
		serviceImages, err := imgMgr.GetImagesForService(ctx, svc, kubeconfig)
		if err != nil {
			Verbose("Warning: failed to extract images for '%s': %v", svc.Name, err)
			serviceImages = []string{}
		}

		if len(serviceImages) > 0 {
			Verbose("Detected %d image(s) for service '%s': %v", len(serviceImages), svc.Name, serviceImages)

			// Get image info and hashes for all detected images
			imageHashes := make(map[string]string)
			localImages := make([]string, 0)

			for _, img := range serviceImages {
				imgInfo, err := imgMgr.GetImageInfo(ctx, img)
				if err != nil {
					Verbose("Warning: failed to get info for image '%s': %v", img, err)
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

			// Separate local images (already in Docker) from remote images (need to pull)
			for _, img := range serviceImages {
				currentHash := imageHashes[img]
				imgInfo, _ := imgMgr.GetImageInfo(ctx, img)

				if imgInfo != nil && imgInfo.IsLocal {
					// Image is local - check if it changed
					if st.HasImageHashChanged(svc.Name, img, currentHash) {
						imagesToLoad = append(imagesToLoad, img)
					} else {
						Verbose("Image '%s' unchanged (hash matches), skipping load", img)
					}
				} else {
					// Image is not local - need to pull it first
					imagesToPull = append(imagesToPull, img)
				}
			}

			// Pull remote images first
			if len(imagesToPull) > 0 {
				fmt.Printf("Pulling %d remote image(s)...\n", len(imagesToPull))
				for _, img := range imagesToPull {
					Verbose("Pulling image '%s'...", img)
					if err := kindMgr.PullImage(ctx, img); err != nil {
						fmt.Printf("Warning: failed to pull image '%s': %v\n", img, err)
					} else {
						Verbose("%s Image '%s' pulled", color.Checkmark(), img)
						// Add to load list after successful pull
						imagesToLoad = append(imagesToLoad, img)
					}
				}
			}

			// Load images that need to be loaded
			if len(imagesToLoad) > 0 {
				fmt.Printf("Loading %d local image(s) into cluster...\n", len(imagesToLoad))

				for _, img := range imagesToLoad {
					Verbose("Loading image '%s'...", img)
					if err := kindMgr.LoadImage(ctx, cfg.Cluster.Name, img); err != nil {
						// Don't fail the installation if image loading fails
						// The image might be available in a registry
						fmt.Printf("Warning: failed to load image '%s': %v\n", img, err)
					} else {
						Verbose("%s Image '%s' loaded", color.Checkmark(), img)
					}
				}

				fmt.Printf("%s Images loaded successfully\n", color.Checkmark())
			} else if len(localImages) > 0 {
				Verbose("All %d local image(s) already loaded (hashes match)", len(localImages))
			}

			// Store image hashes in state for future comparisons
			if len(imageHashes) > 0 {
				// We'll update the state with image hashes after installation
				defer func(serviceName string, hashes map[string]string) {
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
			Verbose("Warning: failed to check if namespace '%s' exists: %v", namespace, err)
			namespaceExists = false
		}

		// We will create the namespace if:
		// 1. It doesn't exist AND
		// 2. create_namespace is true (which is now the default)
		willCreateNamespace := !namespaceExists && svc.ShouldCreateNamespace()

		// Install the service
		if err := provider.Install(ctx, svc); err != nil {
			return fmt.Errorf("failed to install '%s': %w", svc.Name, err)
		}

		// Update state with namespace tracking
		st.MarkServiceInstalledWithNamespace(svc.Name, namespace, willCreateNamespace)
		if err := st.Save(stateFilePath); err != nil {
			Verbose("Warning: failed to save state: %v", err)
		}

		fmt.Printf("%s Service '%s' installed successfully\n", color.Checkmark(), svc.Name)
	}

	fmt.Printf("\n%s All services installed successfully!\n", color.Checkmark())
	fmt.Printf("\nTo check status: kraze status\n")
	fmt.Printf("To tear down:    kraze down\n")

	return nil
}

func init() {
	upCmd.Flags().BoolVar(&upWait, "wait", true, "Wait for services to be ready")
	upCmd.Flags().BoolVar(&upNoWait, "no-wait", false, "Don't wait for services to be ready")
	upCmd.Flags().StringVar(&upTimeout, "timeout", "10m", "Timeout for wait operations")
	upCmd.Flags().StringSliceVarP(&upLabels, "label", "l", []string{}, "Filter services by label (format: key=value, can be specified multiple times)")
}
