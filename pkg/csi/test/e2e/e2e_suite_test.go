/*
Copyright 2025, OpenNebula Project, OpenNebula Systems.

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
package e2e

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/go-connections/nat"
	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/repo"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubectl/pkg/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/kind/pkg/cluster"
)

const (
	kindClusterName          = "mgmt-csi-e2e-cluster"
	workloadClusterName      = "wkld-csi-e2e-cluster"
	workloadClusterNamespace = "default"
	capiProvider             = "kadm"
	caponeChartName          = "capone"
	caponeChartUrl           = "https://opennebula.github.io/cluster-api-provider-opennebula/charts"
	flannelUrl               = "https://raw.githubusercontent.com/flannel-io/flannel/master/Documentation/kube-flannel.yml"
	workloadMachineCount     = 2
	localRegistryPort        = "5005"
	localRegistryName        = "local-registry"
	kubernetesVersion        = "v1.31.4"
	driverName               = "opennebula-csi-plugin"
	driverImageTag           = "e2e"
	publicNetworkName        = "service"
	registryImage            = "registry:2"
)

var (
	oneXMLRPC               string
	oneAuth                 string
	localRegistry           string
	infraestructureProvider string
	workloadTimeout         time.Duration
	basePath                string
)

var (
	ctx                             = ctrl.SetupSignalHandler()
	_, cancelWatches                = context.WithCancel(ctx)
	clusterClient                   clusterctlclient.Client
	managementClusterKubeconfigPath string
)

func init() {
	flag.StringVar(
		&oneXMLRPC,
		"e2e.one-xmlrpc",
		"http://172.20.0.1:2633/RPC2",
		"OpenNebula XMLRPC endpoint for e2e tests",
	)

	flag.StringVar(
		&oneAuth,
		"e2e.one-auth",
		"oneadmin:changeme",
		"OpenNebula authentication for e2e tests",
	)

	flag.StringVar(
		&localRegistry,
		"e2e.local-registry",
		"172.20.0.1",
		"Local Docker registry IP for e2e tests",
	)

	flag.StringVar(
		&infraestructureProvider,
		"e2e.capone-provider",
		"opennebula:v0.1.7",
		"Capone provider version for e2e tests",
	)

	flag.DurationVar(
		&workloadTimeout,
		"e2e.workload-timeout",
		600*time.Second,
		"Timeout for workload cluster readiness",
	)

	flag.StringVar(
		&basePath,
		"e2e.base-path",
		"../../../..",
		"Base path for repository",
	)

}

var _ = SynchronizedBeforeSuite(func() {
	// Download e2e binary
	Expect(getE2Ebinary()).ToNot(HaveOccurred(), "Failed to download e2e binary")

	// Create Manager cluster
	provider := cluster.NewProvider()
	managementClusterKubeconfig, err := os.CreateTemp("", "e2e-kind")
	Expect(err).ToNot(HaveOccurred(), "Failed to create kubeconfig for management cluster")
	managementClusterKubeconfigPath = managementClusterKubeconfig.Name()

	err = provider.Create(
		kindClusterName,
		cluster.CreateWithKubeconfigPath(managementClusterKubeconfigPath),
	)
	Expect(err).ToNot(HaveOccurred(), "Failed to create kind cluster")
	ginkgo.DeferCleanup(func() {
		provider.Delete(kindClusterName, managementClusterKubeconfig.Name())
		os.Remove(managementClusterKubeconfigPath)
	})

	// Create a local Docker registry
	err = createLocalRegistry()
	Expect(err).ToNot(HaveOccurred(), "Failed to create local Docker registry")

	err = buildPushCSIDriverImage(
		fmt.Sprintf("%s:%s/%s:%s", localRegistry, localRegistryPort, driverName, driverImageTag),
		basePath,
		"Dockerfile",
	)
	Expect(err).ToNot(HaveOccurred(), "Failed to build and push CSI driver image")

	clusterClient, err = clusterctlclient.New(ctx, "")
	Expect(err).NotTo(HaveOccurred(), "Failed to create clusterctl client")

	initOptions := clusterctlclient.InitOptions{
		Kubeconfig:              clusterctlclient.Kubeconfig{Path: managementClusterKubeconfigPath},
		InfrastructureProviders: []string{infraestructureProvider},
		WaitProviders:           true,
	}

	_, err = clusterClient.Init(ctx, initOptions)
	Expect(err).NotTo(HaveOccurred(), "Failed to initialize clusterctl client")

	// Create a workload cluster using capone charts
	settings := cli.New()
	settings.KubeConfig = managementClusterKubeconfigPath

	err = addRepo(caponeChartName, caponeChartUrl, settings)
	Expect(err).ToNot(HaveOccurred(), "Failed to add capone chart repository")

	chartValues := map[string]interface{}{
		"ONE_XMLRPC":           oneXMLRPC,
		"ONE_AUTH":             oneAuth,
		"WORKER_MACHINE_COUNT": workloadMachineCount,
		// E2E tests connect to nodes using the private network
		"PRIVATE_NETWORK_NAME": publicNetworkName,
	}

	err = installChart(workloadClusterName,
		fmt.Sprintf("%s/%s-%s", caponeChartName, caponeChartName, capiProvider),
		workloadClusterNamespace,
		settings,
		chartValues)
	Expect(err).ToNot(HaveOccurred(), "Failed to install capone-kadm chart")

	// Wait until workload cluster is up and running
	workloadKubeconfig, err := waitWorkloadClusterReady(managementClusterKubeconfigPath, clusterClient)
	Expect(err).ToNot(HaveOccurred(), "Failed to wait for workload cluster to be ready")

	chartValues = map[string]interface{}{
		"oneAuth":        oneAuth,
		"oneApiEndpoint": oneXMLRPC,
		"image": map[string]interface{}{
			"repository": fmt.Sprintf("%s:%s/%s", localRegistry, localRegistryPort, driverName),
			"tag":        driverImageTag,
			"pullPolicy": "IfNotPresent",
		},
	}

	// Install CSI driver on the workload cluster
	settings.KubeConfig = workloadKubeconfig
	err = installChart("capone-csi",
		fmt.Sprintf("%s/helm/%s", basePath, driverName),
		workloadClusterNamespace,
		settings,
		chartValues)
	Expect(err).ToNot(HaveOccurred(), "Failed to install capone-csi chart")

}, func() {})

var _ = SynchronizedAfterSuite(func() {}, func() {
	// Delete workload cluster
	config, err := clientcmd.BuildConfigFromFlags("", managementClusterKubeconfigPath)
	clusterv1.AddToScheme(scheme.Scheme)

	deleteClient, err := ctrlclient.New(config, ctrlclient.Options{
		Scheme: scheme.Scheme,
	})
	Expect(err).ToNot(HaveOccurred(), "Failed to create controller-runtime client")
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadClusterName,
			Namespace: workloadClusterNamespace,
		},
	}

	err = deleteClient.Delete(ctx, cluster)
	Expect(err).ToNot(HaveOccurred(), "Failed to delete workload cluster")

	// Wait for the workload cluster to be deleted
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	ctxWorkload, cancel := context.WithTimeout(ctx, workloadTimeout)
	defer cancel()

	deleted := false
	for !deleted {
		select {
		case <-ticker.C:
			err = deleteClient.Get(ctx, ctrlclient.ObjectKey{
				Name:      workloadClusterName,
				Namespace: workloadClusterNamespace,
			}, cluster)
			if apierrors.IsNotFound(err) {
				log.Println("Workload cluster deleted successfully")
				deleted = true
			} else if err != nil {
				log.Printf("Error checking workload cluster deletion: %v", err)
			}
		case <-ctxWorkload.Done():
			log.Println("Context cancelled while waiting for workload cluster deletion")
			Expect(ctxWorkload.Err()).ToNot(HaveOccurred(), "Context cancelled while waiting for workload cluster deletion")
		}
	}

	// Delete registry
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	Expect(err).ToNot(HaveOccurred(), "Failed to create Docker client")
	defer cli.Close()

	listOptions := container.ListOptions{
		All: true,
	}
	containers, err := cli.ContainerList(ctx, listOptions)
	Expect(err).ToNot(HaveOccurred(), "Failed to list Docker containers")

	containerRemoveOptions := container.RemoveOptions{
		Force: true,
	}
	for _, container := range containers {
		if container.Names[0] == "/"+localRegistryName {
			err = cli.ContainerRemove(ctx, container.ID, containerRemoveOptions)
			Expect(err).ToNot(HaveOccurred(), "Failed to remove local registry container")
		}
	}

	// Delete e2e image and prune
	imageRemoveOptions := image.RemoveOptions{
		Force:         true,
		PruneChildren: true,
	}
	_, err = cli.ImageRemove(context.Background(),
		fmt.Sprintf("%s:%s/%s:%s", localRegistry, localRegistryPort, driverName, driverImageTag), imageRemoveOptions)
	Expect(err).ToNot(HaveOccurred(), "Failed to remove e2e image")

})

func addRepo(name, url string, settings *cli.EnvSettings) error {
	repoFile := settings.RepositoryConfig

	c := repo.Entry{
		Name: name,
		URL:  url,
	}

	providers := getter.All(settings)
	r, err := repo.NewChartRepository(&c, providers)
	if err != nil {
		return err
	}

	_, err = r.DownloadIndexFile()
	if err != nil {
		return err
	}

	f := repo.NewFile()
	f.Update(&c)
	return f.WriteFile(repoFile, 0644)
}

func installChart(releaseName, chartName, namespace string, settings *cli.EnvSettings, vals map[string]interface{}) error {
	actionConfig := new(action.Configuration)
	err := actionConfig.Init(settings.RESTClientGetter(), namespace, "memory", log.Printf)
	if err != nil {
		return err
	}

	client := action.NewInstall(actionConfig)
	client.ReleaseName = releaseName
	client.Namespace = namespace
	client.CreateNamespace = true

	cp, err := client.ChartPathOptions.LocateChart(chartName, settings)
	if err != nil {
		return err
	}

	chart, err := loader.Load(cp)
	if err != nil {
		return err
	}

	templates, ok := chart.Values["CLUSTER_TEMPLATES"]
	if ok {
		templateList, ok := templates.([]interface{})
		if ok {
			base64Config := generateBase64RegistryConfig()
			for _, template := range templateList {
				templateMap, ok := template.(map[string]interface{})
				if ok {
					if _, exists := templateMap["templateContent"]; exists {
						if name, exists := templateMap["templateName"]; exists {
							if strings.Contains(name.(string), "worker") ||
								strings.Contains(name.(string), "master") {
								templateMap["templateContent"] = applyBase64Code(
									base64Config,
								)
							}
						}
					}
				}
			}
		}
	}

	_, err = client.Run(chart, vals)
	return err
}

func waitWorkloadClusterReady(kubeconfigPath string, client clusterctlclient.Client) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), workloadTimeout)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timeout waiting for workload cluster to be ready")
		case <-ticker.C:
			ready, workloadKubeconfigPath, _ := checkWorkloadClusterReady(kubeconfigPath, client)
			if ready {
				return workloadKubeconfigPath, nil
			}
		}
	}
}

func checkWorkloadClusterReady(kubeconfigPath string, client clusterctlclient.Client) (bool, string, error) {
	getKubeconfigOptions := clusterctlclient.GetKubeconfigOptions{
		Kubeconfig: clusterctlclient.Kubeconfig{
			Path: kubeconfigPath,
		},
		Namespace:           workloadClusterNamespace,
		WorkloadClusterName: workloadClusterName,
	}
	workloadKubeconfig, err := client.GetKubeconfig(ctx, getKubeconfigOptions)
	if err != nil {
		return false, "", fmt.Errorf("failed to get workload kubeconfig: %w", err)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig([]byte(workloadKubeconfig))
	if err != nil {
		return false, "", fmt.Errorf("failed to build REST config from kubeconfig bytes: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return false, "", fmt.Errorf("failed to create clientset: %w", err)
	}

	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, "", fmt.Errorf("failed to list nodes: %w", err)
	}
	if len(nodes.Items) != workloadMachineCount+1 {
		return false, "", fmt.Errorf("expected %d nodes, got %d", workloadMachineCount+1, len(nodes.Items))
	}

	kubeconfigFilePath := filepath.Join(os.TempDir(), fmt.Sprintf("%s-kubeconfig.yaml", workloadClusterName))
	ginkgo.DeferCleanup(func() {
		os.Remove(kubeconfigFilePath)
	})

	err = os.WriteFile(kubeconfigFilePath, []byte(workloadKubeconfig), 0600)
	if err != nil {
		return false, "", fmt.Errorf("failed to save kubeconfig to file: %w", err)
	}

	err = applyManifestFromURL(kubeconfigFilePath, flannelUrl)
	if err != nil {
		return false, "", fmt.Errorf("failed to apply flannel manifest: %w", err)
	}

	for _, node := range nodes.Items {
		for _, condition := range node.Status.Conditions {
			if condition.Type == "Ready" && condition.Status != "True" {
				return false, "", fmt.Errorf("node %s is not ready", node.Name)
			}
		}
	}

	return true, kubeconfigFilePath, nil
}

func createLocalRegistry() error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("failed to create Docker client: %w", err)
	}
	defer cli.Close()

	// Check if container exists
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf("failed to list Docker containers: %w", err)
	}
	for _, container := range containers {
		if container.Names[0] == "/"+localRegistryName {
			log.Printf("Container %s already exists, skipping creation", localRegistryName)
			return nil
		}
	}

	out, err := cli.ImagePull(ctx, registryImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %w", registryImage, err)
	}
	defer out.Close()
	io.Copy(io.Discard, out)

	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image: registryImage,
			ExposedPorts: nat.PortSet{
				"5000/tcp": struct{}{},
			},
		},
		&container.HostConfig{
			PortBindings: nat.PortMap{
				"5000/tcp": []nat.PortBinding{
					{
						HostIP:   "0.0.0.0",
						HostPort: localRegistryPort,
					},
				},
			},
			RestartPolicy: container.RestartPolicy{
				Name: "always",
			},
		},
		&network.NetworkingConfig{},
		nil,
		localRegistryName,
	)
	if err != nil {
		return fmt.Errorf("failed to create Docker container: %w", err)
	}
	// Start the container
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start Docker container: %w", err)
	}
	return nil
}

func buildPushCSIDriverImage(imageName, contextDir, dockerfilePath string) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("failed to create Docker client: %w", err)
	}
	defer cli.Close()

	tarContext, err := archive.TarWithOptions(contextDir, &archive.TarOptions{
		Compression: archive.Uncompressed,
	})
	if err != nil {
		return fmt.Errorf("failed to create tar context: %w", err)
	}
	defer tarContext.Close()

	buildImage, err := cli.ImageBuild(ctx, tarContext, types.ImageBuildOptions{
		Dockerfile:  dockerfilePath,
		Tags:        []string{imageName},
		Target:      driverName,
		Remove:      true,
		ForceRemove: true,
		Platform:    "linux/amd64",
	})
	if err != nil {
		return fmt.Errorf("failed to build image: %w", err)
	}
	if _, err := io.Copy(os.Stdout, buildImage.Body); err != nil {
		return fmt.Errorf("failed to read build output: %w", err)
	}

	defer buildImage.Body.Close()

	err = waitForImage(ctx, cli, imageName, 30*time.Second)
	if err != nil {
		return fmt.Errorf("failed to wait for image: %w", err)
	}

	pushImage, err := cli.ImagePush(ctx, imageName, image.PushOptions{
		RegistryAuth: base64.URLEncoding.EncodeToString([]byte(`{}`)),
	})
	if err != nil {
		return fmt.Errorf("failed to push image: %w", err)
	}
	if _, err := io.Copy(os.Stdout, pushImage); err != nil {
		return fmt.Errorf("failed to read push output: %w", err)
	}
	defer pushImage.Close()
	return nil
}

func generateBase64RegistryConfig() string {
	registry := fmt.Sprintf("%s:%s", localRegistry, localRegistryPort)

	template := `install -m u=rw,go=r -D /dev/fd/0 /etc/containerd/certs.d/%s/hosts.toml <<EOF
[host."http://%s"]
  capabilities = ["pull", "resolve"]
  skip_verify = true
EOF
systemctl restart containerd
`
	content := fmt.Sprintf(template, registry, registry, registry)
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	return fmt.Sprintf(`START_SCRIPT_BASE64 = "%s"`, encoded)
}

func applyBase64Code(base64config string) string {
	template := `CONTEXT = [
USERNAME = "$UNAME",
BACKEND = "YES",
NETWORK = "YES",
GROW_FS = "/",
SET_HOSTNAME = "$NAME",
SSH_PUBLIC_KEY = "$USER[SSH_PUBLIC_KEY]",
TOKEN = "YES",
%s ]
CPU = "1"
DISK = [
IMAGE = "{{ .Release.Name }}-node",
SIZE = "16384" ]
GRAPHICS = [
LISTEN = "0.0.0.0",
TYPE = "vnc" ]
HYPERVISOR = "kvm"
LXD_SECURITY_PRIVILEGED = "true"
MEMORY = "3072"
OS = [
ARCH = "x86_64",
FIRMWARE_SECURE = "YES" ]
SCHED_REQUIREMENTS = "HYPERVISOR=kvm"
VCPU = "2"
`
	return fmt.Sprintf(template, base64config)
}

func waitForImage(ctx context.Context, cli *client.Client, imageName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for image %s to be available locally", imageName)
		case <-ticker.C:
			_, err := cli.ImageInspect(ctx, imageName)
			if err == nil {
				return nil
			}
		}
	}
}

func getE2Ebinary() error {
	url := fmt.Sprintf("https://dl.k8s.io/%s/kubernetes-test-linux-amd64.tar.gz", kubernetesVersion)
	targetFile := "kubernetes/test/bin/e2e.test"
	destPath := "./e2e.test"
	if _, err := os.Stat(destPath); err == nil {
		log.Printf("File %s already exists, skipping download", destPath)
		return nil
	}

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download e2e binary: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("error HTTP: %d %s", resp.StatusCode, resp.Status)
	}

	gzReader, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("error reading tar: %w", err)
		}

		if header.Name == targetFile && header.Typeflag == tar.TypeReg {
			outFile, err := os.Create(destPath)
			if err != nil {
				return fmt.Errorf("failed to create output file: %w", err)
			}

			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return fmt.Errorf("failed to copy file from tar: %w", err)
			}
			outFile.Close()

			if err := os.Chmod(destPath, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("failed to set file permissions: %w", err)
			}
			return nil
		}
	}
	return nil
}

func applyManifestFromURL(kubeconfig, url string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to build config from kubeconfig: %w", err)
	}

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download manifest: %w", err)
	}
	defer resp.Body.Close()

	dc, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create discovery client: %w", err)
	}

	gr, err := restmapper.GetAPIGroupResources(dc)
	if err != nil {
		return fmt.Errorf("failed to get API group resources: %w", err)
	}

	mapper := restmapper.NewDiscoveryRESTMapper(gr)

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	decoder := yaml.NewYAMLOrJSONDecoder(resp.Body, 4096)

	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err != nil {
			break
		}

		gvk := obj.GroupVersionKind()
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			continue
		}

		var dr dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			ns := obj.GetNamespace()
			if ns == "" {
				ns = "default"
			}
			dr = dynClient.Resource(mapping.Resource).Namespace(ns)
		} else {
			dr = dynClient.Resource(mapping.Resource)
		}

		_, err = dr.Create(context.TODO(), &obj, metav1.CreateOptions{})
		if err != nil && strings.Contains(err.Error(), "already exists") {
			_, err = dr.Update(context.TODO(), &obj, metav1.UpdateOptions{})
		}

		if err != nil {
			return fmt.Errorf("failed to apply manifest: %w", err)
		}
	}

	return nil
}

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	ctrl.SetLogger(GinkgoLogr)
	RunSpecs(t, "csi-e2e")
}
