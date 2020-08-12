// Copyright 2019 Layer5.io
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package linkerd

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/restmapper"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"

	"github.com/linkerd/linkerd2/expose/cmd"

	"github.com/fatih/color"
	getter "github.com/hashicorp/go-getter"
	"github.com/layer5io/meshery-linkerd/pkg/util"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	emojivotoInstallFile = "https://run.linkerd.io/emojivoto.yml"
	booksAppInstallFile  = "https://run.linkerd.io/booksapp.yml"
	cachePeriod          = 1 * time.Hour
	jsonOutput           = "json"
	tableOutput          = "table"
	wideOutput           = "wide"
)

var (
	emojivotoLocalFile = path.Join(os.TempDir(), "emojivoto.yml")
	booksAppLocalFile  = path.Join(os.TempDir(), "booksapp.yml")

	stdout = color.Output
	stderr = color.Error
)

func (iClient *Client) downloadFile(urlToDownload, localFile string) error {
	dFile, err := os.Create(localFile)
	if err != nil {
		err = errors.Wrapf(err, "unable to create a file on the filesystem at %s", localFile)
		logrus.Error(err)
		return err
	}

	defer util.SafeClose(dFile, &err)
	err = getter.GetFile(localFile, urlToDownload)
	if err != nil {
		err = errors.Wrapf(err, "Download the file failed %s", localFile)
		logrus.Error(err)
		return err
	}
	/* #nosec */
	err = os.Chmod(localFile, 0755)
	if err != nil {
		err = errors.Wrapf(err, "unable to change permission on %s", localFile)
		logrus.Error(err)
		return err
	}
	return nil
}

func (iClient *Client) getYAML(remoteURL, localFile string) (string, error) {

	proceedWithDownload := true

	lFileStat, err := os.Stat(localFile)
	if err == nil {
		if time.Since(lFileStat.ModTime()) > cachePeriod {
			proceedWithDownload = true
		} else {
			proceedWithDownload = false
		}
	}

	if proceedWithDownload {
		// TODO Change to the HashiCorp tool which uses in the shipyard-run repo
		if err = iClient.downloadFile(remoteURL, localFile); err != nil {
			return "", err
		}
		logrus.Debug("file successfully downloaded . . .")
	}
	/* #nosec */
	b, err := ioutil.ReadFile(localFile)
	return string(b), err
}

func (iClient *Client) preCheck(namspace string) (string, error) {
	// Do linkerd check command
	options := cmd.NewCheckOptions(false, true, false, false, jsonOutput, "stable-2.8.1")
	installManifest, err := cmd.ExposeConfigAndRunChecks(stdout, stderr, "", namspace, options)
	if err != nil {
		return "", err
	}
	return installManifest, nil
}

func (iClient *Client) deployment(deploymentYAML string) error {
	// Because the Linkerd2 used Helm v2.16.8, so we can not use Helm v3 because there may too much conflict in the dependency
	acceptedK8sTypes := regexp.MustCompile(`(Namespace|Role|ClusterRole|RoleBinding|ClusterRoleBinding|ServiceAccount|MutatingWebhookConfiguration|Secret|ValidatingWebhookConfiguration|APIService|PodSecurityPolicy|ConfigMap|Service|Deployment|CronJob|CustomResourceDefinition)`)
	sepYamlfiles := strings.Split(deploymentYAML, "\n---\n")
	// Command out for private kubebuilder which use the runtime.Object
	//retVal := make([]runtime.Object, 0, len(sepYamlfiles))
	for _, f := range sepYamlfiles {
		if f == "\n" || f == "" {
			// ignore empty cases
			continue
		}

		// Need to manually add the resources to the scheme &_&
		sch := runtime.NewScheme()
		_ = scheme.AddToScheme(sch)
		_ = apiextv1beta1.AddToScheme(sch)
		_ = apiregistrationv1.AddToScheme(sch)
		decode := serializer.NewCodecFactory(sch).UniversalDeserializer().Decode

		//decode := clientgoscheme.Codecs.UniversalDeserializer().Decode
		obj, groupVersionKind, err := decode([]byte(f), nil, nil)

		if err != nil {
			logrus.Debug(fmt.Sprintf("Error while decoding YAML object. Err was: %s", err))
			continue
		}

		if !acceptedK8sTypes.MatchString(groupVersionKind.Kind) {
			logrus.Debug(fmt.Sprintf("The custom-roles configMap contained K8s object types which are not supported! Skipping object with type: %s", groupVersionKind.Kind))
		} else {
			// convert the runtime.Object to unstructured.Unstructured
			gk := schema.GroupKind{
				Group: groupVersionKind.Group,
				Kind:  groupVersionKind.Kind,
			}
			groupResources, err := restmapper.GetAPIGroupResources(iClient.k8sClientset.Discovery())
			if err != nil {
				return nil
			}
			resm := restmapper.NewDiscoveryRESTMapper(groupResources)
			mapping, err := resm.RESTMapping(gk, groupVersionKind.Version)
			if err != nil {
				return nil
			}
			logrus.Debug(mapping)

			unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)

			if err != nil {
				return err
			}
			data := &unstructured.Unstructured{}
			data.SetUnstructuredContent(unstructuredObj)
			logrus.Debug(unstructuredObj)

			if mapping.Scope.Name() == "root" {
				_, err = iClient.k8sDynamicClient.Resource(mapping.Resource).Create(data, metav1.CreateOptions{})
			} else {
				_, err = iClient.k8sDynamicClient.Resource(mapping.Resource).Namespace(data.GetNamespace()).Create(data, metav1.CreateOptions{})
			}
			if err != nil {
				logrus.Info(err)
				return err
			}
		}
	}
	return nil
}
