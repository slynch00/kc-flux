package manifests

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"

	"github.com/fluxcd/flux/pkg/image"
	"github.com/fluxcd/flux/pkg/resource"
)

const (
	ConfigFilename = ".flux.yaml"
	CommandTimeout = time.Minute
)

// ConfigFile holds the values necessary for generating and updating
// manifests according to a `.flux.yaml` file. It does double duty as
// the format for the file (to deserialise into), and the state
// necessary for running commands.
type ConfigFile struct {
	Version int
	// Only one of the following should be set simultaneously
	CommandUpdated *CommandUpdated `yaml:"commandUpdated"`
	PatchUpdated   *PatchUpdated   `yaml:"patchUpdated"`

	path       string // the absolute path to the .flux.yaml
	workingDir string // the absolute path to the dir in which to run commands or find a patch file
}

// CommandUpdated represents a config in which updates are done by
// execing commands as given.
type CommandUpdated struct {
	Generators []Generator
	Updaters   []Updater
}

// Generator is an individual command for generating manifests.
type Generator struct {
	Command string
}

// Updater gives a means for updating image refs and a means for
// updating policy in a manifest.
type Updater struct {
	ContainerImage ContainerImageUpdater `yaml:"containerImage"`
	Policy         PolicyUpdater
}

// ContainerImageUpdater is a command for updating the image used by a
// container, in a manifest.
type ContainerImageUpdater struct {
	Command string
}

// PolicyUpdater is a command for updating a policy for a manifest.
type PolicyUpdater struct {
	Command string
}

// PatchUpdated represents a config in which updates are done by
// maintaining a patch, which is calculating from, and applied to, the
// generated manifests.
type PatchUpdated struct {
	Generators []Generator
	PatchFile  string `yaml:"patchFile"`
}

// NewConfigFile constructs a ConfigFile from the file at the absolute
// path given, with the absolute working dir given.
func NewConfigFile(path, workingDir string) (*ConfigFile, error) {
	var result ConfigFile
	fileBytes, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read: %s", err)
	}
	if err := yaml.Unmarshal(fileBytes, &result); err != nil {
		return nil, fmt.Errorf("cannot parse: %s", err)
	}
	result.path = path
	result.workingDir = workingDir
	switch {
	case result.Version != 1:
		return nil, errors.New("incorrect version, only version 1 is supported for now")
	case (result.CommandUpdated != nil && result.PatchUpdated != nil) ||
		(result.CommandUpdated == nil && result.PatchUpdated == nil):
		return nil, errors.New("a single commandUpdated or patchUpdated entry must be defined")
	case result.PatchUpdated != nil && result.PatchUpdated.PatchFile == "":
		return nil, errors.New("patchUpdated's patchFile cannot be empty")
	}
	return &result, nil
}

// -- entry points for using a config file to generate or update manifests

func makeNoCommandsRunErr(field string, cf *ConfigFile) error {
	relConfigPath, err := cf.RelativeConfigPath()
	if err != nil {
		return fmt.Errorf("config file not relative to working dir: %s", err)
	}
	return fmt.Errorf("no %s commands to run in %s", field, relConfigPath)
}

// RelativeConfigPath returns the path to the config file, relative to
// the working directory. This is used in error messages and to
// identify resources generated from the config file.
func (cf *ConfigFile) RelativeConfigPath() (string, error) {
	return filepath.Rel(cf.workingDir, cf.path)
}

// GenerateManifests returns the manifests generated (and patched, if
// necessary) according to the config file.
func (cf *ConfigFile) GenerateManifests(ctx context.Context, manifests Manifests) ([]byte, error) {
	if cf.PatchUpdated != nil {
		_, finalBytes, _, err := cf.getGeneratedAndPatchedManifests(ctx, manifests)
		return finalBytes, err
	}
	return cf.getGeneratedManifests(ctx, manifests, cf.CommandUpdated.Generators)
}

func (cf *ConfigFile) SetWorkloadContainerImage(ctx context.Context, manifests Manifests, r resource.Resource, container string, newImageID image.Ref) error {
	if cf.PatchUpdated != nil {
		return cf.updatePatchFile(ctx, manifests, func(previousManifests []byte) ([]byte, error) {
			return manifests.SetWorkloadContainerImage(previousManifests, r.ResourceID(), container, newImageID)
		})
	}

	// Command-updated
	result := cf.execContainerImageUpdaters(ctx, r.ResourceID(), container, newImageID.Name.String(), newImageID.Tag)
	if len(result) == 0 {
		return makeNoCommandsRunErr("update.containerImage", cf)
	}

	if len(result) > 0 && result[len(result)-1].Error != nil {
		updaters := cf.CommandUpdated.Updaters
		return fmt.Errorf("error executing image updater command %q from file %q: %s\noutput:\n%s",
			updaters[len(result)-1].ContainerImage.Command,
			result[len(result)-1].Error,
			r.Source(),
			result[len(result)-1].Output,
		)
	}
	return nil
}

// UpdateWorkloadPolicies updates policies for a workload, using
// commands or patching according to the config file.
func (cf *ConfigFile) UpdateWorkloadPolicies(ctx context.Context, manifests Manifests, r resource.Resource, update resource.PolicyUpdate) (bool, error) {
	if cf.PatchUpdated != nil {
		var changed bool
		err := cf.updatePatchFile(ctx, manifests, func(previousManifests []byte) ([]byte, error) {
			updatedManifests, err := manifests.UpdateWorkloadPolicies(previousManifests, r.ResourceID(), update)
			if err == nil {
				changed = bytes.Compare(previousManifests, updatedManifests) != 0
			}
			return updatedManifests, err
		})
		return changed, err
	}

	// Command-updated
	workload, ok := r.(resource.Workload)
	if !ok {
		return false, errors.New("resource " + r.ResourceID().String() + " does not have containers")
	}
	changes, err := resource.ChangesForPolicyUpdate(workload, update)
	if err != nil {
		return false, err
	}

	for key, value := range changes {
		result := cf.execPolicyUpdaters(ctx, r.ResourceID(), key, value)
		if len(result) == 0 {
			return false, makeNoCommandsRunErr("updaters.policy", cf)
		}

		if len(result) > 0 && result[len(result)-1].Error != nil {
			updaters := cf.CommandUpdated.Updaters
			err := fmt.Errorf("error executing annotation updater command %q from file %q: %s\noutput:\n%s",
				updaters[len(result)-1].Policy.Command,
				result[len(result)-1].Error,
				r.Source(),
				result[len(result)-1].Output,
			)
			return false, err
		}
	}
	// We assume that the update changed the resource. Alternatively, we could generate the resources
	// again and compare the output, but that's expensive.
	return true, nil
}

type ConfigFileExecResult struct {
	Error  error
	Stderr []byte
	Stdout []byte
}

type ConfigFileCombinedExecResult struct {
	Error  error
	Output []byte
}

// -- these are helpers to support the entry points above

// getGeneratedAndPatchedManifests is used to generate manifests when
// the config is patchUpdated.
func (cf *ConfigFile) getGeneratedAndPatchedManifests(ctx context.Context, manifests Manifests) ([]byte, []byte, string, error) {
	generatedManifests, err := cf.getGeneratedManifests(ctx, manifests, cf.PatchUpdated.Generators)
	if err != nil {
		return nil, nil, "", err
	}

	// The patch file is given in the config file as a path relative
	// to the working directory
	relPatchFilePath := cf.PatchUpdated.PatchFile
	patchFilePath := filepath.Join(cf.workingDir, relPatchFilePath)

	patch, err := ioutil.ReadFile(patchFilePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, nil, "", err
		}
		// Tolerate a missing patch file, since it may not have been created yet.
		// However, its base path must exist.
		patchBaseDir := filepath.Dir(patchFilePath)
		if stat, err := os.Stat(patchBaseDir); err != nil || !stat.IsDir() {
			err := fmt.Errorf("base directory (%q) of patchFile (%q) does not exist",
				filepath.Dir(relPatchFilePath), relPatchFilePath)
			return nil, nil, "", err
		}
		patch = nil
	}
	relConfigFilePath, err := cf.RelativeConfigPath()
	if err != nil {
		return nil, nil, "", err
	}
	patchedManifests, err := manifests.ApplyManifestPatch(generatedManifests, patch, relConfigFilePath, relPatchFilePath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("processing %q, cannot apply patchFile %q to generated resources: %s", relConfigFilePath, relPatchFilePath, err)
	}
	return generatedManifests, patchedManifests, patchFilePath, nil
}

// getGeneratedManifests is used to produce the manifests based _only_
// on the generators in the config. This is sufficient for
// commandUpdated config, and the first step for patchUpdated config.
func (cf *ConfigFile) getGeneratedManifests(ctx context.Context, manifests Manifests, generators []Generator) ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	for i, cmdResult := range cf.execGenerators(ctx, generators) {
		relConfigFilePath, err := cf.RelativeConfigPath()
		if err != nil {
			return nil, err
		}
		if cmdResult.Error != nil {
			err := fmt.Errorf("error executing generator command %q from file %q: %s\nerror output:\n%s\ngenerated output:\n%s",
				generators[i].Command,
				relConfigFilePath,
				cmdResult.Error,
				string(cmdResult.Stderr),
				string(cmdResult.Stderr),
			)
			return nil, err
		}
		if err := manifests.AppendManifestToBuffer(cmdResult.Stdout, buf); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// updatePatchFile calculates the patch given a transformation, and
// updates the patch file given in the config.
func (cf *ConfigFile) updatePatchFile(ctx context.Context, manifests Manifests, updateFn func(previousManifests []byte) ([]byte, error)) error {
	generatedManifests, patchedManifests, patchFilePath, err := cf.getGeneratedAndPatchedManifests(ctx, manifests)
	if err != nil {
		relConfigFilePath, err := cf.RelativeConfigPath()
		if err != nil {
			return err
		}
		return fmt.Errorf("error parsing generated, patched output from file %s: %s", relConfigFilePath, err)
	}
	finalManifests, err := updateFn(patchedManifests)
	if err != nil {
		return err
	}
	newPatch, err := manifests.CreateManifestPatch(generatedManifests, finalManifests, "generated manifests", "patched and updated manifests")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(patchFilePath, newPatch, 0600)
}

// execGenerators executes all the generators given and returns the
// results; it will stop at the first failing command.
func (cf *ConfigFile) execGenerators(ctx context.Context, generators []Generator) []ConfigFileExecResult {
	result := []ConfigFileExecResult{}
	for _, g := range generators {
		stdErr := bytes.NewBuffer(nil)
		stdOut := bytes.NewBuffer(nil)
		err := cf.execCommand(ctx, nil, stdOut, stdErr, g.Command)
		r := ConfigFileExecResult{
			Stdout: stdOut.Bytes(),
			Stderr: stdErr.Bytes(),
			Error:  err,
		}
		result = append(result, r)
		// Stop executing on the first command error
		if err != nil {
			break
		}
	}
	return result
}

// execContainerImageUpdaters executes all the image updates in the configuration file.
// It will stop at the first error, in which case the returned error will be non-nil
func (cf *ConfigFile) execContainerImageUpdaters(ctx context.Context,
	workload resource.ID, container string, image, imageTag string) []ConfigFileCombinedExecResult {
	env := makeEnvFromResourceID(workload)
	env = append(env,
		"FLUX_CONTAINER="+container,
		"FLUX_IMG="+image,
		"FLUX_TAG="+imageTag,
	)
	commands := []string{}
	var updaters []Updater
	if cf.CommandUpdated != nil {
		updaters = cf.CommandUpdated.Updaters
	}
	for _, u := range updaters {
		commands = append(commands, u.ContainerImage.Command)
	}
	return cf.execCommandsWithCombinedOutput(ctx, env, commands)
}

// execPolicyUpdaters executes all the policy update commands given in
// the configuration file. An empty policyValue means remove the
// policy. It will stop at the first error, in which case the returned
// error will be non-nil
func (cf *ConfigFile) execPolicyUpdaters(ctx context.Context,
	workload resource.ID, policyName, policyValue string) []ConfigFileCombinedExecResult {
	env := makeEnvFromResourceID(workload)
	env = append(env, "FLUX_POLICY="+policyName)
	if policyValue != "" {
		env = append(env, "FLUX_POLICY_VALUE="+policyValue)
	}
	commands := []string{}
	var updaters []Updater
	if cf.CommandUpdated != nil {
		updaters = cf.CommandUpdated.Updaters
	}
	for _, u := range updaters {
		commands = append(commands, u.Policy.Command)
	}
	return cf.execCommandsWithCombinedOutput(ctx, env, commands)
}

func (cf *ConfigFile) execCommandsWithCombinedOutput(ctx context.Context, env []string, commands []string) []ConfigFileCombinedExecResult {
	env = append(env, "PATH="+os.Getenv("PATH"))
	result := []ConfigFileCombinedExecResult{}
	for _, c := range commands {
		stdOutAndErr := bytes.NewBuffer(nil)
		err := cf.execCommand(ctx, env, stdOutAndErr, stdOutAndErr, c)
		r := ConfigFileCombinedExecResult{
			Output: stdOutAndErr.Bytes(),
			Error:  err,
		}
		result = append(result, r)
		// Stop executing on the first command error
		if err != nil {
			break
		}
	}
	return result
}

func (cf *ConfigFile) execCommand(ctx context.Context, env []string, stdOut, stdErr io.Writer, command string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = env
	cmd.Dir = cf.workingDir
	cmd.Stdout = stdOut
	cmd.Stderr = stdErr
	err := cmd.Run()
	if cmdCtx.Err() == context.DeadlineExceeded {
		err = cmdCtx.Err()
	} else if cmdCtx.Err() == context.Canceled {
		err = errors.Wrap(ctx.Err(), fmt.Sprintf("context was unexpectedly cancelled"))
	}
	return err
}

func makeEnvFromResourceID(id resource.ID) []string {
	ns, kind, name := id.Components()
	return []string{
		"FLUX_WORKLOAD=" + id.String(),
		"FLUX_WL_NS=" + ns,
		"FLUX_WL_KIND=" + kind,
		"FLUX_WL_NAME=" + name,
	}
}
