/*
Copyright 2019 The Skaffold Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package deploy

import (
	"context"
	"io"
	"io/ioutil"
	"os/exec"
	"path/filepath"

	yaml "gopkg.in/yaml.v2"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/build"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/color"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/constants"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/kubectl"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/event"
	runcontext "github.com/GoogleContainerTools/skaffold/pkg/skaffold/runner/context"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
	"github.com/pkg/errors"
)

// kustomization is the content of a kustomization.yaml file.
type kustomization struct {
	Bases              []string             `yaml:"bases"`
	Resources          []string             `yaml:"resources"`
	Patches            []string             `yaml:"patches"`
	CRDs               []string             `yaml:"crds"`
	PatchesJSON6902    []patchJSON6902      `yaml:"patchesJson6902"`
	ConfigMapGenerator []configMapGenerator `yaml:"configMapGenerator"`
	SecretGenerator    []secretGenerator    `yaml:"secretGenerator"`
}

type patchJSON6902 struct {
	Path string `yaml:"path"`
}

type configMapGenerator struct {
	Files []string `yaml:"files"`
}

type secretGenerator struct {
	Files []string `yaml:"files"`
}

// KustomizeDeployer deploys workflows using kustomize CLI.
type KustomizeDeployer struct {
	*latest.KustomizeDeploy

	kubectl     kubectl.CLI
	defaultRepo string
}

func NewKustomizeDeployer(runCtx *runcontext.RunContext) *KustomizeDeployer {
	return &KustomizeDeployer{
		KustomizeDeploy: runCtx.Cfg.Deploy.KustomizeDeploy,
		kubectl: kubectl.CLI{
			Namespace:   runCtx.Opts.Namespace,
			KubeContext: runCtx.KubeContext,
			Flags:       runCtx.Cfg.Deploy.KustomizeDeploy.Flags,
		},
		defaultRepo: runCtx.DefaultRepo,
	}
}

// Labels returns the labels specific to kustomize.
func (k *KustomizeDeployer) Labels() map[string]string {
	return map[string]string{
		constants.Labels.Deployer: "kustomize",
	}
}

// Deploy runs `kubectl apply` on the manifest generated by kustomize.
func (k *KustomizeDeployer) Deploy(ctx context.Context, out io.Writer, builds []build.Artifact, labellers []Labeller) error {
	color.Default.Fprintln(out, "kubectl client version:", k.kubectl.Version(ctx))
	if err := k.kubectl.CheckVersion(ctx); err != nil {
		color.Default.Fprintln(out, err)
	}

	manifests, err := k.readManifests(ctx)
	if err != nil {
		event.DeployFailed(err)
		return errors.Wrap(err, "reading manifests")
	}

	if len(manifests) == 0 {
		return nil
	}

	event.DeployInProgress()

	manifests, err = manifests.ReplaceImages(builds, k.defaultRepo)
	if err != nil {
		event.DeployFailed(err)
		return errors.Wrap(err, "replacing images in manifests")
	}

	manifests, err = manifests.SetLabels(merge(labellers...))
	if err != nil {
		event.DeployFailed(err)
		return errors.Wrap(err, "setting labels in manifests")
	}

	err = k.kubectl.Apply(ctx, out, manifests)
	if err != nil {
		event.DeployFailed(err)
	}

	event.DeployComplete()
	return nil
}

// Cleanup deletes what was deployed by calling Deploy.
func (k *KustomizeDeployer) Cleanup(ctx context.Context, out io.Writer) error {
	manifests, err := k.readManifests(ctx)
	if err != nil {
		return errors.Wrap(err, "reading manifests")
	}

	if err := k.kubectl.Delete(ctx, out, manifests); err != nil {
		return errors.Wrap(err, "delete")
	}

	return nil
}

func dependenciesForKustomization(dir string) ([]string, error) {
	var deps []string

	path := filepath.Join(dir, "kustomization.yaml")
	buf, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := kustomization{}
	if err := yaml.Unmarshal(buf, &content); err != nil {
		return nil, err
	}

	for _, base := range content.Bases {
		baseDeps, err := dependenciesForKustomization(filepath.Join(dir, base))
		if err != nil {
			return nil, err
		}

		deps = append(deps, baseDeps...)
	}

	deps = append(deps, path)
	deps = append(deps, joinPaths(dir, content.Resources)...)
	deps = append(deps, joinPaths(dir, content.Patches)...)
	deps = append(deps, joinPaths(dir, content.CRDs)...)
	for _, patch := range content.PatchesJSON6902 {
		deps = append(deps, filepath.Join(dir, patch.Path))
	}
	for _, generator := range content.ConfigMapGenerator {
		deps = append(deps, joinPaths(dir, generator.Files)...)
	}
	for _, generator := range content.SecretGenerator {
		deps = append(deps, joinPaths(dir, generator.Files)...)
	}

	return deps, nil
}

func joinPaths(root string, paths []string) []string {
	var list []string

	for _, path := range paths {
		list = append(list, filepath.Join(root, path))
	}

	return list
}

// Dependencies lists all the files that can change what needs to be deployed.
func (k *KustomizeDeployer) Dependencies() ([]string, error) {
	return dependenciesForKustomization(k.KustomizePath)
}

func (k *KustomizeDeployer) readManifests(ctx context.Context) (kubectl.ManifestList, error) {
	cmd := exec.CommandContext(ctx, "kustomize", "build", k.KustomizePath)
	out, err := util.RunCmdOut(cmd)
	if err != nil {
		return nil, errors.Wrap(err, "kustomize build")
	}

	if len(out) == 0 {
		return nil, nil
	}

	var manifests kubectl.ManifestList
	manifests.Append(out)
	return manifests, nil
}
