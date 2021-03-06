/*
Copyright 2020 Red Hat

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

/*
This tool implements /usr/local/bin/csv-generator for the operator image,
which is an interfaced defined in openstack-cluster-operator:

  https://github.com/openstack-k8s-operators/openstack-cluster-operator

It outputs a ClusterServiceVersion with some modifications for use in an
openstack deployment. It uses the ClusterService version generated by
operator-sdk as its base. The changes can be summarised as:

- Convert the operator to run in a single namespace.
- Substitute various parameters which are passed on the command line.
*/

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/blang/semver"
	"github.com/ghodss/yaml"
	"github.com/operator-framework/api/pkg/lib/version"
	csvv1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

var (
	versionArg               = flag.String("csv-version", "", "")
	replacesVersionedNameArg = flag.String("replaces-csv-version", "", "")
	namespaceArg             = flag.String("namespace", "", "")
	pullPolicyArg            = flag.String("pull-policy", "Always", "")

	logoBase64Arg = flag.String("logo-base64", "", "")
	verbosityArg  = flag.String("verbosity", "", "")

	operatorImageArg = flag.String("operator-image-name", "quay.io/openstack-k8s-operators/ovn-central-operator:devel", "optional")
)

const (
	operatorName = "ovn-central-operator"
)

type args struct {
	version               semver.Version
	replacesVersionedName string
	namespace             string
	pullPolicy            corev1.PullPolicy
	operatorImage         string
}

func parseArgs() (*args, error) {
	flag.Parse()

	args := &args{}

	if *versionArg == "" {
		return nil, fmt.Errorf("--csv-version must be specified")
	}
	if version, err := semver.New(*versionArg); err != nil {
		return nil, fmt.Errorf("Error parsing version %s: %w", *versionArg, err)
	} else {
		args.version = *version
	}

	if *namespaceArg == "" {
		return nil, fmt.Errorf("--namespace must be specified")
	}
	args.namespace = *namespaceArg

	args.replacesVersionedName = *replacesVersionedNameArg
	args.operatorImage = *operatorImageArg

	pullPolicy := func() (corev1.PullPolicy, error) {
		switch strings.ToLower(*pullPolicyArg) {
		case "always":
			return corev1.PullAlways, nil
		case "never":
			return corev1.PullNever, nil
		case "ifnotpresent":
			return corev1.PullIfNotPresent, nil
		}

		return corev1.PullPolicy(""), fmt.Errorf("Invalid pull policy %s", *pullPolicyArg)
	}

	var err error
	if args.pullPolicy, err = pullPolicy(); err != nil {
		return nil, err
	}

	if *logoBase64Arg != "" {
		return nil, fmt.Errorf("WARNING: setting logo is not implemented")
	}

	if *verbosityArg != "" {
		return nil, fmt.Errorf("WARNING: setting verbosity is not implemented")
	}

	return args, nil
}

func exit(err error) {
	fmt.Fprintf(os.Stderr, "%s\n", err)
	os.Exit(1)
}

func main() {
	args, err := parseArgs()
	if err != nil {
		exit(err)
	}

	csv := &csvv1.ClusterServiceVersion{}
	if err := unmarshalCSV(csv); err != nil {
		exit(err)
	}

	modifyCSV(csv, args)

	bytes, err := marshalCSV(csv)
	if err != nil {
		exit(err)
	}

	fmt.Printf("%s", bytes)
}

func unmarshalCSV(csv *csvv1.ClusterServiceVersion) error {
	baseCSVPath, found := os.LookupEnv("BASE_CSV")
	if !found {
		return fmt.Errorf("BASE_CSV is not set")
	}

	baseCSVBytes, err := ioutil.ReadFile(baseCSVPath)
	if err != nil {
		return err
	}

	jsonBytes, err := k8syaml.ToJSON(baseCSVBytes)
	if err != nil {
		return err
	}

	return json.Unmarshal(jsonBytes, csv)
}

func marshalCSV(csv *csvv1.ClusterServiceVersion) ([]byte, error) {
	jsonBytes, err := json.Marshal(csv)
	if err != nil {
		return nil, err
	}

	return yaml.JSONToYAML(jsonBytes)
}

func modifyCSV(csv *csvv1.ClusterServiceVersion, args *args) {
	csv.Name = fmt.Sprintf("%s.%s", operatorName, args.version)
	csv.Namespace = args.namespace
	csv.Spec.Version = version.OperatorVersion{Version: args.version}
	csv.Spec.Replaces = args.replacesVersionedName

	installStrategySpec := &csv.Spec.InstallStrategy.StrategySpec

	// We don't specify a namespace in the the source code kubebuilder
	// annotations, which generates a ClusterRole. Here we convert that into a
	// namespaced Role.
	installStrategySpec.Permissions = installStrategySpec.ClusterPermissions
	installStrategySpec.ClusterPermissions = nil

	// Because we don't use a ClusterRole, the only install mode we support is
	// SingleNamespace
	foundSingle := false
	installModes := &csv.Spec.InstallModes
	for i := 0; i < len(*installModes); i++ {
		installMode := &(*installModes)[i]
		if installMode.Type == csvv1.InstallModeTypeSingleNamespace {
			installMode.Supported = true
			foundSingle = true
		} else {
			installMode.Supported = false
		}
	}
	if !foundSingle {
		*installModes = append(*installModes, csvv1.InstallMode{
			Type:      csvv1.InstallModeTypeSingleNamespace,
			Supported: true,
		})
	}

	// Find the manager container in the operator deployment
	for i := 0; i < len(installStrategySpec.DeploymentSpecs); i++ {
		deploymentSpec := &installStrategySpec.DeploymentSpecs[i]

		containers := deploymentSpec.Spec.Template.Spec.Containers
		for j := 0; j < len(containers); j++ {
			container := &containers[j]
			if container.Name != "manager" {
				continue
			}

			// Update image and pull policy
			container.Image = args.operatorImage
			container.ImagePullPolicy = args.pullPolicy

			// Add WATCH_NAMESPACE environment variable
			var watchNS *corev1.EnvVar
			for k := 0; k < len(container.Env); k++ {
				env := &container.Env[i]
				if env.Name != "WATCH_NAMESPACE" {
					continue
				}
				watchNS = env
			}
			if watchNS == nil {
				container.Env = append(container.Env, corev1.EnvVar{Name: "WATCH_NAMESPACE"})
				watchNS = &container.Env[len(container.Env)-1]
			}
			watchNS.Value = ""
			watchNS.ValueFrom = &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
			}
		}
	}
}
