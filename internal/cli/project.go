package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/hjames9/kraze/internal/color"
	"github.com/hjames9/kraze/internal/config"
	"github.com/hjames9/kraze/internal/state"
	"go.yaml.in/yaml/v3"
)

// checkProjectAccess warns about unowned services on the cluster that this
// project won't claim. They are entries written before projects existed, which
// another project claims once its config contains them. configured is every
// service in the config, not only those a command was asked to act on: a
// service merely left out of this run is still claimed by a later one.
func checkProjectAccess(st *state.ClusterState, cfg *config.Config, configured map[string]bool) {
	var foreign []string
	for _, name := range st.UnownedServices() {
		if !configured[name] {
			foreign = append(foreign, name)
		}
	}
	if len(foreign) > 0 {
		fmt.Printf("%s Warning: cluster '%s' has service(s) with no project: %s. "+
			"They are claimed by the first project whose config contains them.\n",
			color.Warning(), cfg.Cluster.Name, strings.Join(foreign, ", "))
	}
}

// claimServices adopts unowned state entries for the config's services into
// the current project (see state.ClusterState.Claim)
func claimServices(st *state.ClusterState, services map[string]config.ServiceConfig) {
	for name := range services {
		if st.Claim(name) {
			Verbose("Claimed unowned service '%s' for project '%s'", name, st.Project())
		}
	}
}

// checkServiceConflicts fails if another project owns a service with the same
// name in the same namespace as one of ours. Helm release names are the
// service name, so installing it would upgrade the other project's release.
// An owner that was set up from one of cfgPaths is this config under its old
// project name, so the error explains how to rename a project instead.
func checkServiceConflicts(st *state.ClusterState, services map[string]config.ServiceConfig, project string, cfgPaths []string) error {
	var conflicts []string
	renamedFrom := ""
	storedPaths := st.AllProjectConfigPaths()
	for name, svc := range services {
		owner, ok := st.ConflictingOwner(name, svc.GetNamespace())
		if !ok {
			continue
		}
		if owner != "" && slices.ContainsFunc(storedPaths[owner], func(p string) bool { return slices.Contains(cfgPaths, p) }) {
			renamedFrom = owner
		}
		conflicts = append(conflicts, fmt.Sprintf("'%s' in namespace '%s' is owned by project %s", name, svc.GetNamespace(), projectLabel(owner)))
	}
	if len(conflicts) == 0 {
		return nil
	}
	sort.Strings(conflicts)
	hint := "rename the service or move it to a different namespace. If this project was renamed, " +
		"remove its services under the old name first: set the old `project:` again and run `kraze down`"
	if renamedFrom != "" {
		hint = fmt.Sprintf("project '%s' was set up from this same config, so it looks like it was renamed to '%s'. "+
			"Its services must be removed under the old name first: set `project: %s` again, run `kraze down`, "+
			"then set `project: %s` and run `kraze up`", renamedFrom, project, renamedFrom, project)
	}
	return fmt.Errorf("service name conflict with another project on this cluster:\n  %s\n%s", strings.Join(conflicts, "\n  "), hint)
}

// clusterConfigHash fingerprints the settings that only take effect when a
// cluster is created, so a later `up` can warn that they are being ignored
func clusterConfigHash(cluster config.ClusterConfig) string {
	cluster.Name = ""
	data, err := yaml.Marshal(cluster)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// warnClusterConfigDrift warns when this config's cluster settings differ from
// those the existing cluster was created with. A state with no recorded hash
// (created by an older kraze) adopts the current one without a warning, and
// this project becomes its creator if none is recorded.
func warnClusterConfigDrift(st *state.ClusterState, cfg *config.Config) {
	hash := clusterConfigHash(cfg.Cluster)
	if hash == "" {
		return
	}
	if st.ClusterConfigHash == "" {
		createdBy := st.CreatedBy
		if createdBy == "" {
			createdBy = cfg.Project
		}
		st.SetClusterConfig(hash, createdBy)
		return
	}
	if st.ClusterConfigHash == hash {
		return
	}

	createdBy := "a different configuration"
	if st.CreatedBy != "" {
		createdBy = fmt.Sprintf("project '%s'", st.CreatedBy)
	}
	fmt.Printf("%s Warning: cluster settings in this config differ from those cluster '%s' was created with (by %s). "+
		"They are ignored; run `kraze destroy` and `kraze up` to apply them.\n",
		color.Warning(), cfg.Cluster.Name, createdBy)
}

// checkGPUCompatibility reports GPU settings that the existing cluster can't
// satisfy. GPU support is fixed at creation. The project that created the
// cluster must match exactly, so that changing the setting prompts a
// recreate; so must any project on a cluster whose creator isn't recorded
// (created by an older kraze, which had no projects). Another project only
// fails if it needs a GPU vendor the cluster lacks.
func checkGPUCompatibility(st *state.ClusterState, cfg *config.Config) error {
	nvidiaEnabled := cfg.Cluster.GPU.IsNvidiaEnabled()
	amdEnabled := cfg.Cluster.GPU.IsAMDEnabled()

	exact := st.CreatedBy == "" || st.CreatedBy == cfg.Project
	mismatch := st.NvidiaGPUEnabled != nvidiaEnabled || st.AMDGPUEnabled != amdEnabled
	if !exact {
		mismatch = (nvidiaEnabled && !st.NvidiaGPUEnabled) || (amdEnabled && !st.AMDGPUEnabled)
	}
	if !mismatch {
		return nil
	}

	return fmt.Errorf(
		"GPU configuration has changed since cluster '%s' was created.\n"+
			"Was:  nvidia(enabled=%v) amd(enabled=%v)\n"+
			"Now:  nvidia(enabled=%v) amd(enabled=%v)\n"+
			"GPU support requires cluster recreation. Run: kraze destroy && kraze up",
		cfg.Cluster.Name,
		st.NvidiaGPUEnabled, st.AMDGPUEnabled,
		nvidiaEnabled, amdEnabled,
	)
}
