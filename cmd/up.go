package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/covexo/devspace/pkg/util/hash"
	"github.com/covexo/devspace/pkg/util/stdinutil"

	"github.com/covexo/devspace/pkg/util/yamlutil"

	"github.com/covexo/devspace/pkg/devspace/config/generated"
	"github.com/covexo/devspace/pkg/devspace/image"

	"github.com/covexo/devspace/pkg/util/log"

	"github.com/covexo/devspace/pkg/devspace/registry"
	synctool "github.com/covexo/devspace/pkg/devspace/sync"

	helmClient "github.com/covexo/devspace/pkg/devspace/deploy/helm"
	"github.com/covexo/devspace/pkg/devspace/kubectl"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/covexo/devspace/pkg/devspace/config/configutil"
	"github.com/spf13/cobra"
	k8sv1 "k8s.io/api/core/v1"
	k8sv1beta1 "k8s.io/api/rbac/v1beta1"
	"k8s.io/client-go/kubernetes"
)

// UpCmd is a struct that defines a command call for "up"
type UpCmd struct {
	flags     *UpCmdFlags
	helm      *helmClient.ClientWrapper
	kubectl   *kubernetes.Clientset
	workdir   string
	pod       *k8sv1.Pod
	container *k8sv1.Container
}

// UpCmdFlags are the flags available for the up-command
type UpCmdFlags struct {
	tiller         bool
	open           string
	initRegistries bool
	build          bool
	sync           bool
	deploy         bool
	portforwarding bool
	noSleep        bool
	verboseSync    bool
	container      string
}

//UpFlagsDefault are the default flags for UpCmdFlags
var UpFlagsDefault = &UpCmdFlags{
	tiller:         true,
	open:           "cmd",
	initRegistries: true,
	build:          false,
	sync:           true,
	deploy:         false,
	portforwarding: true,
	noSleep:        false,
	verboseSync:    false,
	container:      "",
}

const clusterRoleBindingName = "devspace-users"

func init() {
	cmd := &UpCmd{
		flags: UpFlagsDefault,
	}

	cobraCmd := &cobra.Command{
		Use:   "up",
		Short: "Starts your DevSpace",
		Long: `
#######################################################
#################### devspace up ######################
#######################################################
Starts and connects your DevSpace:
1. Connects to the Tiller server
2. Builds your Docker image (if your Dockerfile has changed)
3. Deploys the Helm chart in /chart
4. Starts the sync client
5. Enters the container shell
#######################################################`,
		Run: cmd.Run,
	}
	rootCmd.AddCommand(cobraCmd)

	cobraCmd.Flags().BoolVar(&cmd.flags.tiller, "tiller", cmd.flags.tiller, "Install/upgrade tiller")
	cobraCmd.Flags().BoolVar(&cmd.flags.initRegistries, "init-registries", cmd.flags.initRegistries, "Initialize registries (and install internal one)")
	cobraCmd.Flags().BoolVarP(&cmd.flags.build, "build", "b", cmd.flags.build, "Force image build")
	cobraCmd.Flags().StringVarP(&cmd.flags.container, "container", "c", cmd.flags.container, "Container name where to open the shell")
	cobraCmd.Flags().BoolVar(&cmd.flags.sync, "sync", cmd.flags.sync, "Enable code synchronization")
	cobraCmd.Flags().BoolVar(&cmd.flags.verboseSync, "verbose-sync", cmd.flags.verboseSync, "When enabled the sync will log every file change")
	cobraCmd.Flags().BoolVar(&cmd.flags.portforwarding, "portforwarding", cmd.flags.portforwarding, "Enable port forwarding")
	cobraCmd.Flags().BoolVarP(&cmd.flags.deploy, "deploy", "d", cmd.flags.deploy, "Force chart deployment")
	cobraCmd.Flags().BoolVar(&cmd.flags.noSleep, "no-sleep", cmd.flags.noSleep, "Enable no-sleep (Override the containers.default.command and containers.default.args values with empty strings)")
}

// Run executes the command logic
func (cmd *UpCmd) Run(cobraCmd *cobra.Command, args []string) {
	log.StartFileLogging()

	workdir, err := os.Getwd()
	if err != nil {
		log.Fatalf("Unable to determine current workdir: %s", err.Error())
	}

	cmd.workdir = workdir
	configExists, _ := configutil.ConfigExists()
	if !configExists {
		initCmd := &InitCmd{
			flags: InitCmdFlagsDefault,
		}

		initCmd.Run(nil, []string{})
	}

	cmd.kubectl, err = kubectl.NewClient()
	if err != nil {
		log.Fatalf("Unable to create new kubectl client: %v", err)
	}

	err = cmd.ensureNamespace()
	if err != nil {
		log.Fatalf("Unable to create namespace: %v", err)
	}

	err = cmd.ensureClusterRoleBinding()
	if err != nil {
		log.Fatalf("Unable to create ClusterRoleBinding: %v", err)
	}

	cmd.initHelm()

	if cmd.flags.initRegistries {
		cmd.initRegistries()
	}

	cmd.buildAndDeploy()

	if cmd.flags.portforwarding {
		cmd.startPortForwarding()
	}

	if cmd.flags.sync {
		syncConfigs := cmd.startSync()
		defer func() {
			for _, v := range syncConfigs {
				v.Stop()
			}
		}()
	}

	enterTerminal(cmd.kubectl, cmd.pod, cmd.flags.container, args)
}

func (cmd *UpCmd) buildAndDeploy() {
	// Load config
	generatedConfig, err := generated.LoadConfig()
	if err != nil {
		log.Fatalf("Error loading generated.yaml: %v", err)
	}

	// Build image if necessary
	mustRedeploy := cmd.buildImages(generatedConfig)

	// Check if the chart directory has changed
	hash, err := hash.Directory("chart")
	if err != nil {
		log.Fatalf("Error hashing chart directory: %v", err)
	}

	// Check if we find a running release pod
	pod, err := getRunningDevSpacePod(cmd.helm, cmd.kubectl)
	if err != nil || mustRedeploy || cmd.flags.deploy || generatedConfig.HelmChartHash != hash {
		cmd.deployChart(generatedConfig)

		generatedConfig.HelmChartHash = hash

		// Save Config
		err = generated.SaveConfig(generatedConfig)
		if err != nil {
			log.Fatalf("Error saving config: %v", err)
		}
	} else {
		cmd.pod = pod
	}
}

func (cmd *UpCmd) ensureNamespace() error {
	config := configutil.GetConfig()
	releaseNamespace := *config.DevSpace.Release.Namespace

	_, err := cmd.kubectl.CoreV1().Namespaces().Get(releaseNamespace, metav1.GetOptions{})
	if err != nil {
		log.Infof("Create namespace %s", releaseNamespace)

		// Create release namespace
		_, err = cmd.kubectl.CoreV1().Namespaces().Create(&k8sv1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: releaseNamespace,
			},
		})
	}

	return err
}

func (cmd *UpCmd) ensureClusterRoleBinding() error {
	if kubectl.IsMinikube() {
		return nil
	}

	_, err := cmd.kubectl.RbacV1beta1().ClusterRoleBindings().Get(clusterRoleBindingName, metav1.GetOptions{})
	if err != nil {
		clusterConfig, _ := kubectl.GetClientConfig()
		if clusterConfig.AuthProvider != nil && clusterConfig.AuthProvider.Name == "gcp" {
			createRoleBinding := stdinutil.GetFromStdin(&stdinutil.GetFromStdinParams{
				Question:               "Do you want the ClusterRoleBinding '" + clusterRoleBindingName + "' to be created automatically? (yes|no)",
				DefaultValue:           "yes",
				ValidationRegexPattern: "^(yes)|(no)$",
			})

			if *createRoleBinding == "no" {
				log.Fatal("Please create ClusterRoleBinding '" + clusterRoleBindingName + "' manually")
			}
			username := configutil.String("")

			log.StartWait("Checking gcloud account")
			gcloudOutput, gcloudErr := exec.Command("gcloud", "config", "list", "account", "--format", "value(core.account)").Output()
			log.StopWait()

			if gcloudErr == nil {
				gcloudEmail := strings.TrimSuffix(strings.TrimSuffix(string(gcloudOutput), "\r\n"), "\n")

				if gcloudEmail != "" {
					username = &gcloudEmail
				}
			}

			username = stdinutil.GetFromStdin(&stdinutil.GetFromStdinParams{
				Question:               "What is the email address of your Google Cloud account?",
				DefaultValue:           *username,
				ValidationRegexPattern: ".+",
			})

			rolebinding := &k8sv1beta1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name: clusterRoleBindingName,
				},
				Subjects: []k8sv1beta1.Subject{
					{
						Kind: "User",
						Name: *username,
					},
				},
				RoleRef: k8sv1beta1.RoleRef{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "ClusterRole",
					Name:     "cluster-admin",
				},
			}

			_, err = cmd.kubectl.RbacV1beta1().ClusterRoleBindings().Create(rolebinding)
			if err != nil {
				return err
			}
		} else {
			cfg := configutil.GetConfig()

			if cfg.Cluster.CloudProvider == nil || *cfg.Cluster.CloudProvider == "" {
				log.Warn("Unable to check permissions: If you run into errors, please create the ClusterRoleBinding '" + clusterRoleBindingName + "' as described here: https://devspace.covexo.com/docs/advanced/rbac.html")
			}
		}
	}

	return nil
}

func (cmd *UpCmd) initRegistries() {
	config := configutil.GetConfig()
	registryMap := *config.Registries

	if config.Services != nil && config.Services.InternalRegistry != nil && config.Registries != nil {
		registryConf, regConfExists := registryMap["internal"]
		if !regConfExists {
			log.Fatal("Registry config not found for internal registry")
		}

		log.StartWait("Initializing internal registry")
		err := registry.InitInternalRegistry(cmd.kubectl, cmd.helm, config.Services.InternalRegistry, registryConf)
		log.StopWait()
		if err != nil {
			log.Fatalf("Internal registry error: %v", err)
		}

		err = configutil.SaveConfig()
		if err != nil {
			log.Fatalf("Saving config error: %v", err)
		}

		log.Done("Internal registry started")
	}

	if registryMap != nil {
		for registryName, registryConf := range registryMap {
			if registryConf.Auth != nil && registryConf.Auth.Password != nil {
				username := ""
				password := *registryConf.Auth.Password
				email := "noreply@devspace-cloud.com"
				registryURL := ""

				if registryConf.Auth.Username != nil {
					username = *registryConf.Auth.Username
				}
				if registryConf.URL != nil {
					registryURL = *registryConf.URL
				}

				log.StartWait("Creating image pull secret for registry: " + registryName)
				err := registry.CreatePullSecret(cmd.kubectl, *config.DevSpace.Release.Namespace, registryURL, username, password, email)
				log.StopWait()

				if err != nil {
					log.Fatalf("Failed to create pull secret for registry: %v", err)
				}
			}
		}
	}
}

// returns true when one of the images had to be rebuild
func (cmd *UpCmd) buildImages(generatedConfig *generated.Config) bool {
	re := false
	config := configutil.GetConfig()

	for imageName, imageConf := range *config.Images {
		shouldRebuild, err := image.Build(cmd.kubectl, generatedConfig, imageName, imageConf, cmd.flags.build)
		if err != nil {
			log.Fatal(err)
		}

		if shouldRebuild {
			re = true
		}
	}

	return re
}

func (cmd *UpCmd) initHelm() {
	if cmd.helm == nil {
		log.StartWait("Initializing helm client")
		defer log.StopWait()

		client, err := helmClient.NewClient(cmd.kubectl, false)
		if err != nil {
			log.Fatalf("Error initializing helm client: %s", err.Error())
		}

		cmd.helm = client
		log.Done("Initialized helm client")
	}
}

func (cmd *UpCmd) deployChart(generatedConfig *generated.Config) {
	config := configutil.GetConfig()

	log.StartWait("Deploying helm chart")
	defer log.StopWait()

	releaseName := *config.DevSpace.Release.Name
	releaseNamespace := *config.DevSpace.Release.Namespace
	chartPath := "chart/"

	values := map[interface{}]interface{}{}
	overwriteValues := map[interface{}]interface{}{}

	err := yamlutil.ReadYamlFromFile(chartPath+"values.yaml", values)
	if err != nil {
		log.Fatalf("Couldn't deploy chart, error reading from chart values %s: %v", chartPath+"values.yaml", err)
	}

	containerValues := map[string]interface{}{}

	for imageName, imageConf := range *config.Images {
		container := map[string]interface{}{}
		container["image"] = registry.GetImageURL(generatedConfig, imageConf, true)

		if cmd.flags.noSleep {
			container["command"] = []string{}
			container["args"] = []string{}
		}

		containerValues[imageName] = container
	}

	pullSecrets := []interface{}{}
	existingPullSecrets, pullSecretsExisting := values["pullSecrets"]

	if pullSecretsExisting {
		pullSecrets = existingPullSecrets.([]interface{})
	}

	for _, registryConf := range *config.Registries {
		if registryConf.URL != nil {
			registrySecretName := registry.GetRegistryAuthSecretName(*registryConf.URL)
			pullSecrets = append(pullSecrets, registrySecretName)
		}
	}

	overwriteValues["containers"] = containerValues
	overwriteValues["pullSecrets"] = pullSecrets

	appRelease, err := cmd.helm.InstallChartByPath(releaseName, releaseNamespace, chartPath, &overwriteValues)
	if err != nil {
		log.Fatalf("Unable to deploy helm chart: %s", err.Error())
	}

	releaseRevision := int(appRelease.Version)
	log.Donef("Deployed helm chart (Release revision: %d)", releaseRevision)
	log.StartWait("Waiting for release pod to become ready")

	cmd.pod, err = helmClient.WaitForReleasePodToGetReady(cmd.kubectl, releaseName, releaseNamespace, releaseRevision)
	if err != nil {
		log.Fatal(err)
	}
}

func (cmd *UpCmd) startSync() []*synctool.SyncConfig {
	config := configutil.GetConfig()
	syncConfigs := make([]*synctool.SyncConfig, 0, len(*config.DevSpace.Sync))

	for _, syncPath := range *config.DevSpace.Sync {
		absLocalPath, err := filepath.Abs(*syncPath.LocalSubPath)

		if err != nil {
			log.Panicf("Unable to resolve localSubPath %s: %s", *syncPath.LocalSubPath, err.Error())
		} else {
			// Retrieve pod from label selector
			labels := make([]string, 0, len(*syncPath.LabelSelector))

			for key, value := range *syncPath.LabelSelector {
				labels = append(labels, key+"="+*value)
			}

			namespace := *config.DevSpace.Release.Namespace
			if syncPath.Namespace != nil && *syncPath.Namespace != "" {
				namespace = *syncPath.Namespace
			}

			pod, err := kubectl.GetFirstRunningPod(cmd.kubectl, strings.Join(labels, ", "), namespace)

			if err != nil {
				log.Panicf("Unable to list devspace pods: %s", err.Error())
			} else if pod != nil {
				if len(pod.Spec.Containers) == 0 {
					log.Warnf("Cannot start sync on pod, because selected pod %s/%s has no containers", pod.Namespace, pod.Name)
					continue
				}

				container := &pod.Spec.Containers[0]
				if syncPath.ContainerName != nil && *syncPath.ContainerName != "" {
					found := false

					for _, c := range pod.Spec.Containers {
						if c.Name == *syncPath.ContainerName {
							container = &c
							found = true
							break
						}
					}

					if found == false {
						log.Warnf("Couldn't start sync, because container %s wasn't found in pod %s/%s", *syncPath.ContainerName, pod.Namespace, pod.Name)
						continue
					}
				}

				syncConfig := &synctool.SyncConfig{
					Kubectl:   cmd.kubectl,
					Pod:       pod,
					Container: container,
					WatchPath: absLocalPath,
					DestPath:  *syncPath.ContainerPath,
					Verbose:   cmd.flags.verboseSync,
				}

				if syncPath.ExcludePaths != nil {
					syncConfig.ExcludePaths = *syncPath.ExcludePaths
				}

				if syncPath.DownloadExcludePaths != nil {
					syncConfig.DownloadExcludePaths = *syncPath.DownloadExcludePaths
				}

				if syncPath.UploadExcludePaths != nil {
					syncConfig.UploadExcludePaths = *syncPath.UploadExcludePaths
				}

				err = syncConfig.Start()
				if err != nil {
					log.Fatalf("Sync error: %s", err.Error())
				}

				log.Donef("Sync started on %s <-> %s (Pod: %s/%s)", absLocalPath, *syncPath.ContainerPath, pod.Namespace, pod.Name)
				syncConfigs = append(syncConfigs, syncConfig)
			}
		}
	}

	return syncConfigs
}

func (cmd *UpCmd) startPortForwarding() {
	config := configutil.GetConfig()

	for _, portForwarding := range *config.DevSpace.PortForwarding {
		if portForwarding.ResourceType == nil || *portForwarding.ResourceType == "pod" {
			if len(*portForwarding.LabelSelector) > 0 {
				labels := make([]string, 0, len(*portForwarding.LabelSelector))

				for key, value := range *portForwarding.LabelSelector {
					labels = append(labels, key+"="+*value)
				}

				namespace := *config.DevSpace.Release.Namespace
				if portForwarding.Namespace != nil && *portForwarding.Namespace != "" {
					namespace = *portForwarding.Namespace
				}

				pod, err := kubectl.GetFirstRunningPod(cmd.kubectl, strings.Join(labels, ", "), namespace)

				if err != nil {
					log.Errorf("Unable to list devspace pods: %s", err.Error())
				} else if pod != nil {
					ports := make([]string, len(*portForwarding.PortMappings))

					for index, value := range *portForwarding.PortMappings {
						ports[index] = strconv.Itoa(*value.LocalPort) + ":" + strconv.Itoa(*value.RemotePort)
					}

					readyChan := make(chan struct{})

					go kubectl.ForwardPorts(cmd.kubectl, pod, ports, make(chan struct{}), readyChan)

					// Wait till forwarding is ready
					select {
					case <-readyChan:
						log.Donef("Port forwarding started on %s", strings.Join(ports, ", "))
					case <-time.After(5 * time.Second):
						log.Error("Timeout waiting for port forwarding to start")
					}
				}
			}
		} else {
			log.Warn("Currently only pod resource type is supported for portforwarding")
		}
	}
}
