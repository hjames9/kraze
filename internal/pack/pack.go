package pack

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/providers"
	"gopkg.in/yaml.v3"
)

const (
	bundleDir       = ".krazepack"
	MetadataFile    = "kraze-package.json"
	metadataVersion = "1"
)

// Asset represents a single local file to include in the archive.
type Asset struct {
	AbsPath  string // absolute path on disk
	ArchPath string // path within archive, forward-slash-separated, relative to archive root
}

// PackageMetadata is stored as kraze-package.json inside the archive.
type PackageMetadata struct {
	Version      string   `json:"version"`
	KrazeVersion string   `json:"kraze_version"`
	CreatedAt    string   `json:"created_at"`
	ConfigFiles  []string `json:"config_files"`
}

// remoteChartInfo tracks a pulled Helm chart .tgz.
type remoteChartInfo struct {
	tgzPath  string // absolute path to downloaded .tgz on disk
	archPath string // path within archive (forward slashes, relative to archive root)
}

// remoteManifestFile is a single downloaded HTTP manifest file.
type remoteManifestFile struct {
	origURL  string // original HTTP URL (used to rewrite the config)
	content  []byte
	archPath string // path within archive (forward slashes, relative to archive root)
}

// remoteManifestInfo groups all downloaded files for one service.
// A service using path: <url> produces one file; a service using paths: [...]
// with multiple HTTP URLs produces one file per URL.
type remoteManifestInfo struct {
	files []remoteManifestFile
}

// CommonAncestor returns the deepest directory that is a common parent of all
// given file paths. All paths must be absolute.
func CommonAncestor(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("no paths provided")
	}

	dirs := make([][]string, len(paths))
	for i, p := range paths {
		dirs[i] = strings.Split(filepath.Dir(filepath.Clean(p)), string(filepath.Separator))
	}

	if len(dirs) == 1 {
		return strings.Join(dirs[0], string(filepath.Separator)), nil
	}

	minLen := len(dirs[0])
	for _, d := range dirs[1:] {
		if len(d) < minLen {
			minLen = len(d)
		}
	}

	commonLen := 0
	for i := 0; i < minLen; i++ {
		part := dirs[0][i]
		allMatch := true
		for _, d := range dirs[1:] {
			if d[i] != part {
				allMatch = false
				break
			}
		}
		if !allMatch {
			break
		}
		commonLen = i + 1
	}

	if commonLen == 0 {
		return "", fmt.Errorf("config files have no common ancestor directory")
	}

	result := strings.Join(dirs[0][:commonLen], string(filepath.Separator))
	if result == "" {
		result = string(filepath.Separator) // filesystem root
	}

	// Refuse to use the filesystem root as archive root — it indicates config
	// files are in completely unrelated directories.
	if result == string(filepath.Separator) {
		return "", fmt.Errorf("config files have no common project directory; ensure all config files are under the same project root")
	}

	return result, nil
}

// CollectLocalAssets gathers all locally-referenced files (charts, manifests,
// values files, CA certs) from cfg. archiveRoot is the common ancestor of all
// config files; paths are made relative to it. firstConfigDir is used to resolve
// ca_certificate paths (which ResolvePaths in parser.go does not handle).
// Remote charts and HTTP manifests are skipped — they are handled separately.
//
// Assets that live outside archiveRoot are automatically bundled under
// .krazepack/external/<service-name>/ rather than causing an error. The returned
// externalPaths map (origAbsPath → archPath) lets rewriteConfig update the
// config YAML to point at the bundled copies.
//
// CA certificates are the exception: they must remain inside the project because
// they reference system-level paths and an out-of-project location is almost
// always a configuration mistake.
func CollectLocalAssets(archiveRoot, firstConfigDir string, cfg *config.Config) ([]Asset, map[string]string, error) {
	seen := make(map[string]bool)
	var assets []Asset
	externalPaths := make(map[string]string) // origAbsPath → archPath in archive

	// addPath adds absPath (file or directory tree) to assets.
	// extArchBase: if non-empty, assets outside archiveRoot are placed under this
	// archive path prefix instead of causing an error.
	addPath := func(absPath, label, extArchBase string) error {
		absPath = filepath.Clean(absPath)
		if seen[absPath] {
			return nil
		}

		info, err := os.Stat(absPath)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}

		if info.IsDir() {
			isExt := extArchBase != "" && isOutsideArchiveRoot(archiveRoot, absPath)
			if isExt {
				externalPaths[absPath] = extArchBase
			}
			return filepath.WalkDir(absPath, func(p string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.IsDir() {
					return nil
				}
				p = filepath.Clean(p)
				if seen[p] {
					return nil
				}
				seen[p] = true
				var archPath string
				if isExt {
					rel, err2 := filepath.Rel(absPath, p)
					if err2 != nil {
						return fmt.Errorf("%s: %w", label, err2)
					}
					archPath = extArchBase + "/" + filepath.ToSlash(rel)
				} else {
					var relErr error
					archPath, relErr = relArchivePath(archiveRoot, p)
					if relErr != nil {
						return fmt.Errorf("%s: %w", label, relErr)
					}
				}
				assets = append(assets, Asset{AbsPath: p, ArchPath: archPath})
				return nil
			})
		}

		seen[absPath] = true
		var archPath string
		if extArchBase != "" && isOutsideArchiveRoot(archiveRoot, absPath) {
			archPath = extArchBase + "/" + filepath.Base(absPath)
			externalPaths[absPath] = archPath
		} else {
			var relErr error
			archPath, relErr = relArchivePath(archiveRoot, absPath)
			if relErr != nil {
				return fmt.Errorf("%s: %w", label, relErr)
			}
		}
		assets = append(assets, Asset{AbsPath: absPath, ArchPath: archPath})
		return nil
	}

	// CA certificates — NOT resolved by ResolvePaths, resolve relative to firstConfigDir.
	// Must be inside the project; an outside path is almost certainly a mistake.
	for _, cert := range cfg.Cluster.CACertificates {
		abs := cert
		if !filepath.IsAbs(cert) {
			abs = filepath.Join(firstConfigDir, cert)
		}
		if err := addPath(abs, "ca_certificate "+cert, ""); err != nil {
			return nil, nil, err
		}
	}

	// Services
	for name, svc := range cfg.Services {
		if !svc.IsEnabled() {
			continue
		}

		// Local Helm chart directory — may live anywhere.
		if svc.IsLocalChart() {
			extBase := ""
			if isOutsideArchiveRoot(archiveRoot, svc.Path) {
				extBase = bundleDir + "/charts/" + name
			}
			if err := addPath(svc.Path, "service "+name+" path", extBase); err != nil {
				return nil, nil, err
			}
		}

		// Manifest files/dirs (skip HTTP URLs — downloaded separately).
		if svc.IsManifests() {
			if svc.Path != "" && !config.IsHTTPURL(svc.Path) {
				extBase := ""
				if isOutsideArchiveRoot(archiveRoot, svc.Path) {
					extBase = bundleDir + "/manifests/" + name
				}
				if err := addPath(svc.Path, "service "+name+" path", extBase); err != nil {
					return nil, nil, err
				}
			}
			for i, p := range svc.Paths {
				if !config.IsHTTPURL(p) {
					extBase := ""
					if isOutsideArchiveRoot(archiveRoot, p) {
						extBase = fmt.Sprintf("%s/manifests/%s-%d", bundleDir, name, i)
					}
					if err := addPath(p, "service "+name+" paths", extBase); err != nil {
						return nil, nil, err
					}
				}
			}
		}

		// Values files
		for _, vf := range svc.Values.Files() {
			if err := addPath(vf, "service "+name+" values", ""); err != nil {
				return nil, nil, err
			}
		}
	}

	return assets, externalPaths, nil
}

// relArchivePath returns a forward-slash-separated path relative to archiveRoot,
// or an error if absPath is outside archiveRoot.
func relArchivePath(archiveRoot, absPath string) (string, error) {
	rel, err := filepath.Rel(archiveRoot, absPath)
	if err != nil {
		return "", fmt.Errorf("computing relative path for %s: %w", absPath, err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %s is outside the project directory %s; move it inside the project first", absPath, archiveRoot)
	}
	return filepath.ToSlash(rel), nil
}

// isOutsideArchiveRoot reports whether absPath lives outside archiveRoot.
func isOutsideArchiveRoot(archiveRoot, absPath string) bool {
	rel, err := filepath.Rel(archiveRoot, absPath)
	return err != nil || strings.HasPrefix(rel, "..")
}

// CreatePackage creates a .tar.gz archive at outputPath containing:
//   - Config files (remote sources rewritten to .krazepack/ local paths)
//   - All local assets (charts, manifests, values, CA certs)
//   - Pulled remote Helm charts as .tgz files
//   - Downloaded HTTP manifests
//   - kraze-package.json metadata
//
// The archive is written to a temp file and atomically renamed on success.
func CreatePackage(configPaths []string, cfg *config.Config, krazeVersion string, outputPath string, verbose bool) error {
	archiveRoot, err := CommonAncestor(configPaths)
	if err != nil {
		return err
	}

	// Guard against .krazepack/ collision in the project.
	if _, err := os.Stat(filepath.Join(archiveRoot, bundleDir)); err == nil {
		return fmt.Errorf("directory %q already exists in %s — this name is reserved by kraze pack; rename it first", bundleDir, archiveRoot)
	}

	// Staging directory for pulled charts.
	stagingDir, err := os.MkdirTemp("", "kraze-pack-staging-*")
	if err != nil {
		return fmt.Errorf("creating staging dir: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	// Pull remote Helm charts.
	remoteCharts := make(map[string]remoteChartInfo)
	helmProvider := providers.NewHelmProviderForPacking(verbose)
	for name, svc := range cfg.Services {
		if !svc.IsEnabled() || !svc.IsRemoteChart() {
			continue
		}
		svcCopy := svc
		chartDir := filepath.Join(stagingDir, "charts", name)
		if err := os.MkdirAll(chartDir, 0755); err != nil {
			return fmt.Errorf("creating chart staging dir for %s: %w", name, err)
		}
		if verbose {
			fmt.Printf("  Pulling chart for service %q...\n", name)
		}
		tgzPath, err := helmProvider.PullChartToDir(&svcCopy, chartDir)
		if err != nil {
			return fmt.Errorf("pulling chart for service %s: %w", name, err)
		}
		remoteCharts[name] = remoteChartInfo{
			tgzPath:  tgzPath,
			archPath: bundleDir + "/charts/" + name + ".tgz",
		}
	}

	// Download HTTP manifests (both singular path: and plural paths: entries).
	remoteManifests := make(map[string]remoteManifestInfo)
	for name, svc := range cfg.Services {
		if !svc.IsEnabled() || !svc.IsManifests() {
			continue
		}
		var files []remoteManifestFile

		if config.IsHTTPURL(svc.Path) {
			if verbose {
				fmt.Printf("  Downloading manifest for service %q...\n", name)
			}
			content, err := downloadURL(svc.Path)
			if err != nil {
				return fmt.Errorf("downloading manifest for service %s: %w", name, err)
			}
			files = append(files, remoteManifestFile{
				origURL:  svc.Path,
				content:  content,
				archPath: bundleDir + "/manifests/" + name + ".yaml",
			})
		}

		for i, p := range svc.Paths {
			if !config.IsHTTPURL(p) {
				continue
			}
			if verbose {
				fmt.Printf("  Downloading manifest[%d] for service %q...\n", i, name)
			}
			content, err := downloadURL(p)
			if err != nil {
				return fmt.Errorf("downloading manifest paths[%d] for service %s: %w", i, name, err)
			}
			files = append(files, remoteManifestFile{
				origURL:  p,
				content:  content,
				archPath: fmt.Sprintf("%s/manifests/%s-%d.yaml", bundleDir, name, i),
			})
		}

		if len(files) > 0 {
			remoteManifests[name] = remoteManifestInfo{files: files}
		}
	}

	// Collect local assets (assets outside archiveRoot are placed under .krazepack/external/).
	firstConfigDir := filepath.Dir(configPaths[0])
	localAssets, externalPaths, err := CollectLocalAssets(archiveRoot, firstConfigDir, cfg)
	if err != nil {
		return fmt.Errorf("collecting local assets: %w", err)
	}
	if verbose && len(externalPaths) > 0 {
		for origPath, archPath := range externalPaths {
			fmt.Printf("  Bundling external asset: %s → %s\n", origPath, archPath)
		}
	}

	// Write to a temp file; atomically rename on success.
	tmpOutput := outputPath + ".tmp"
	f, err := os.Create(tmpOutput)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer os.Remove(tmpOutput)

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	// Write config files (with remote sources rewritten).
	for _, cfgPath := range configPaths {
		archCfgPath, err := relArchivePath(archiveRoot, cfgPath)
		if err != nil {
			return err
		}
		rewritten, err := rewriteConfig(cfgPath, archCfgPath, remoteCharts, remoteManifests, externalPaths)
		if err != nil {
			return fmt.Errorf("rewriting %s: %w", cfgPath, err)
		}
		if err := addBytesToTar(tw, rewritten, archCfgPath); err != nil {
			return fmt.Errorf("writing config %s to archive: %w", archCfgPath, err)
		}
	}

	// Write bundled remote charts.
	for name, pc := range remoteCharts {
		if err := addFileToTar(tw, pc.tgzPath, pc.archPath); err != nil {
			return fmt.Errorf("writing bundled chart for %s: %w", name, err)
		}
	}

	// Write bundled HTTP manifests.
	for name, dm := range remoteManifests {
		for _, f := range dm.files {
			if err := addBytesToTar(tw, f.content, f.archPath); err != nil {
				return fmt.Errorf("writing bundled manifest for %s: %w", name, err)
			}
		}
	}

	// Write local assets.
	for _, asset := range localAssets {
		if err := addFileToTar(tw, asset.AbsPath, asset.ArchPath); err != nil {
			return fmt.Errorf("writing asset %s: %w", asset.ArchPath, err)
		}
	}

	// Write metadata.
	configFileArchPaths := make([]string, len(configPaths))
	for i, p := range configPaths {
		rel, _ := relArchivePath(archiveRoot, p)
		configFileArchPaths[i] = rel
	}
	meta := PackageMetadata{
		Version:      metadataVersion,
		KrazeVersion: krazeVersion,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		ConfigFiles:  configFileArchPaths,
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling metadata: %w", err)
	}
	if err := addBytesToTar(tw, metaBytes, MetadataFile); err != nil {
		return fmt.Errorf("writing metadata: %w", err)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("closing tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("closing gzip: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing output file: %w", err)
	}

	return os.Rename(tmpOutput, outputPath)
}

// rewriteConfig reads configPath and returns a YAML copy where remote Helm
// charts, HTTP manifests, and local assets outside the project directory are
// replaced with .krazepack/-relative local paths.
// archConfigPath is the path of this config file within the archive (forward slashes).
// externalLocalPaths maps origAbsPath → archPath for assets bundled under .krazepack/external/.
func rewriteConfig(
	configPath, archConfigPath string,
	remoteCharts map[string]remoteChartInfo,
	remoteManifests map[string]remoteManifestInfo,
	externalLocalPaths map[string]string,
) ([]byte, error) {
	rawBytes, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	if len(remoteCharts) == 0 && len(remoteManifests) == 0 && len(externalLocalPaths) == 0 {
		return rawBytes, nil
	}

	var rawConfig map[string]interface{}
	if err := yaml.Unmarshal(rawBytes, &rawConfig); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}

	rawServices, _ := rawConfig["services"].(map[string]interface{})
	if rawServices == nil {
		return rawBytes, nil
	}

	// Compute the relative path from this config's archive directory to .krazepack/.
	// e.g., for config at "kraze.yml" → archConfigDir = "." → relBundle = ".krazepack"
	//       for config at "services/db.yml" → archConfigDir = "services" → relBundle = "../.krazepack"
	archConfigDir := filepath.ToSlash(filepath.Dir(archConfigPath))
	relBundle, err := filepath.Rel(archConfigDir, bundleDir)
	if err != nil {
		return nil, fmt.Errorf("computing bundle dir relative path: %w", err)
	}
	relBundle = filepath.ToSlash(relBundle)

	modified := false
	for svcName, svcVal := range rawServices {
		svcMap, ok := svcVal.(map[string]interface{})
		if !ok {
			continue
		}

		if _, isRemote := remoteCharts[svcName]; isRemote {
			svcMap["path"] = relBundle + "/charts/" + svcName + ".tgz"
			delete(svcMap, "repo")
			delete(svcMap, "chart")
			delete(svcMap, "version")
			rawServices[svcName] = svcMap
			modified = true
		}

		if info, isRemote := remoteManifests[svcName]; isRemote {
			// Build URL → config-relative local path for every downloaded file.
			urlToLocal := make(map[string]string, len(info.files))
			for _, f := range info.files {
				urlToLocal[f.origURL] = relBundle + strings.TrimPrefix(f.archPath, bundleDir)
			}

			// Rewrite singular path: if it was a URL, replace with local path.
			if pathVal, ok := svcMap["path"].(string); ok {
				if local, replaced := urlToLocal[pathVal]; replaced {
					svcMap["path"] = local
					modified = true
				}
			}

			// Rewrite plural paths: replace each HTTP entry with its local path.
			if pathsVal, ok := svcMap["paths"].([]interface{}); ok {
				for i, entry := range pathsVal {
					if urlStr, ok := entry.(string); ok {
						if local, replaced := urlToLocal[urlStr]; replaced {
							pathsVal[i] = local
							modified = true
						}
					}
				}
			}

			rawServices[svcName] = svcMap
		}

		// Rewrite local paths that were bundled outside the project root.
		if len(externalLocalPaths) > 0 {
			configDir := filepath.Dir(configPath)
			// resolveRaw converts a raw YAML path value (possibly relative to the
			// config file's on-disk directory) to an absolute path for map lookup.
			resolveRaw := func(p string) string {
				if config.IsHTTPURL(p) {
					return p
				}
				if filepath.IsAbs(p) {
					return filepath.Clean(p)
				}
				return filepath.Clean(filepath.Join(configDir, p))
			}

			if pathVal, ok := svcMap["path"].(string); ok {
				if archPath, isExt := externalLocalPaths[resolveRaw(pathVal)]; isExt {
					svcMap["path"] = relBundle + strings.TrimPrefix(archPath, bundleDir)
					modified = true
				}
			}

			if pathsVal, ok := svcMap["paths"].([]interface{}); ok {
				for i, entry := range pathsVal {
					if p, ok := entry.(string); ok {
						if archPath, isExt := externalLocalPaths[resolveRaw(p)]; isExt {
							pathsVal[i] = relBundle + strings.TrimPrefix(archPath, bundleDir)
							modified = true
						}
					}
				}
			}

			if modified {
				rawServices[svcName] = svcMap
			}
		}
	}

	if !modified {
		return rawBytes, nil
	}

	return yaml.Marshal(rawConfig)
}

// MaybeExtract detects if cfgPaths contains a .tar.gz package and extracts it
// to a temp directory. Returns updated config file paths and a cleanup function.
// If no archive is present, returns cfgPaths unchanged with a no-op cleanup.
func MaybeExtract(cfgPaths []string) ([]string, func(), error) {
	noop := func() {}

	var archivePath string
	for _, p := range cfgPaths {
		if isArchivePath(p) {
			archivePath = p
			break
		}
	}

	if archivePath == "" {
		return cfgPaths, noop, nil
	}

	if len(cfgPaths) > 1 {
		return nil, noop, fmt.Errorf("cannot mix a package archive with regular config files; use either -f package.tar.gz or -f kraze.yml")
	}

	tmpDir, err := os.MkdirTemp("", "kraze-pack-*")
	if err != nil {
		return nil, noop, fmt.Errorf("creating temp dir for package extraction: %w", err)
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	if err := extractTar(archivePath, tmpDir); err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("extracting package %s: %w", archivePath, err)
	}

	configFiles, err := readPackageConfigFiles(tmpDir)
	if err != nil {
		cleanup()
		return nil, noop, err
	}

	resolved := make([]string, len(configFiles))
	for i, f := range configFiles {
		resolved[i] = filepath.Join(tmpDir, filepath.FromSlash(f))
	}

	return resolved, cleanup, nil
}

func isArchivePath(p string) bool {
	return strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".tgz")
}

func readPackageConfigFiles(dir string) ([]string, error) {
	metaPath := filepath.Join(dir, MetadataFile)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{"kraze.yml"}, nil
		}
		return nil, fmt.Errorf("reading package metadata: %w", err)
	}

	var meta PackageMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parsing package metadata: %w", err)
	}

	if len(meta.ConfigFiles) == 0 {
		return []string{"kraze.yml"}, nil
	}
	return meta.ConfigFiles, nil
}

func downloadURL(url string) ([]byte, error) {
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

func addFileToTar(tw *tar.Writer, srcPath, archPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}

	header := &tar.Header{
		Name:     archPath,
		Mode:     int64(info.Mode()),
		Size:     info.Size(),
		ModTime:  info.ModTime(),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func addBytesToTar(tw *tar.Writer, content []byte, archPath string) error {
	header := &tar.Header{
		Name:     archPath,
		Mode:     0644,
		Size:     int64(len(content)),
		ModTime:  time.Now().UTC(),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err := tw.Write(content)
	return err
}

func extractTar(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a valid gzip archive: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		// Guard against path traversal attacks.
		cleanName := filepath.Clean(header.Name)
		if strings.HasPrefix(cleanName, "..") {
			return fmt.Errorf("invalid archive entry: path traversal in %q", header.Name)
		}

		destPath := filepath.Join(destDir, filepath.FromSlash(cleanName))

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destPath, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			out.Close()
			if copyErr != nil {
				return copyErr
			}
		}
	}
	return nil
}
