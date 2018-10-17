package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/covexo/devspace/pkg/devspace/config/configutil"
	"github.com/covexo/devspace/pkg/devspace/config/v1"
	"github.com/covexo/devspace/pkg/util/kubeconfig"

	"github.com/covexo/devspace/pkg/util/log"
	"github.com/skratchdot/open-golang/open"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// CheckAuth verifies if the user is logged into the devspace cloud and if not logs the user in
func CheckAuth(provider *Provider) (string, *api.Cluster, *api.AuthInfo, error) {
	if provider.Token == "" {
		return Login(provider)
	}

	return GetClusterConfig(provider)
}

// GetClusterConfig retrieves the cluster and authconfig from the devspace cloud
func GetClusterConfig(provider *Provider) (string, *api.Cluster, *api.AuthInfo, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", provider.Host+GetClusterConfigEndpoint, nil)
	if err != nil {
		return "", nil, nil, err
	}

	req.Header.Set("Authorization", provider.Token)

	resp, err := client.Do(req)
	if err != nil {
		return "", nil, nil, err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", nil, nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return Login(provider)
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, nil, fmt.Errorf("Couldn't retrieve cluster config: %s", body)
	}

	var objmap map[string]*json.RawMessage
	err = json.Unmarshal(body, &objmap)
	if err != nil {
		return "", nil, nil, err
	}

	cluster := api.NewCluster()
	err = json.Unmarshal(*objmap["cluster"], cluster)
	if err != nil {
		return "", nil, nil, err
	}

	authInfo := api.NewAuthInfo()
	err = json.Unmarshal(*objmap["user"], authInfo)
	if err != nil {
		return "", nil, nil, err
	}

	namespace := ""
	err = json.Unmarshal(*objmap["namespace"], &namespace)
	if err != nil {
		return "", nil, nil, err
	}

	return namespace, cluster, authInfo, nil
}

// Login logs the user into the devspace cloud
func Login(provider *Provider) (string, *api.Cluster, *api.AuthInfo, error) {
	ctx := context.Background()
	tokenChannel := make(chan string)

	server := startServer(provider.Host+LoginSuccessEndpoint, tokenChannel)
	open.Start(provider.Host + LoginEndpoint)

	token := <-tokenChannel
	close(tokenChannel)

	err := server.Shutdown(ctx)
	if err != nil {
		return "", nil, nil, err
	}

	providerConfig, err := ParseCloudConfig()
	if err != nil {
		return "", nil, nil, err
	}

	providerConfig[provider.Name].Token = token

	err = SaveCloudConfig(providerConfig)
	if err != nil {
		return "", nil, nil, err
	}

	return GetClusterConfig(providerConfig[provider.Name])
}

// Update updates the cloud provider information if necessary
func Update(providerConfig ProviderConfig, dsConfig *v1.Config, useKubeContext, switchKubeContext bool) error {
	cloudProvider := *dsConfig.Cluster.CloudProvider

	// Don't update anything if we don't use a cloud provider
	if cloudProvider == "" {
		return nil
	}

	provider, ok := providerConfig[cloudProvider]
	if ok == false {
		return fmt.Errorf("Config for cloud provider %s couldn't be found", cloudProvider)
	}

	namespace, cluster, authInfo, err := CheckAuth(provider)
	if err != nil {
		return err
	}

	UpdateDevSpaceConfig(dsConfig, namespace)

	if useKubeContext {
		kubeContext := DevSpaceKubeContextName + "-" + namespace

		err = UpdateKubeConfig(kubeContext, namespace, cluster, authInfo, switchKubeContext)
		if err != nil {
			return err
		}

		dsConfig.Cluster.Namespace = &namespace
		dsConfig.Cluster.KubeContext = configutil.String(kubeContext)
	} else {
		dsConfig.Cluster.APIServer = &cluster.Server
		dsConfig.Cluster.Namespace = &namespace
		dsConfig.Cluster.CaCert = configutil.String(string(cluster.CertificateAuthorityData))

		dsConfig.Cluster.User = &v1.ClusterUser{
			ClientCert: configutil.String(string(authInfo.ClientCertificateData)),
			ClientKey:  configutil.String(string(authInfo.ClientKeyData)),
			Token:      configutil.String(string(authInfo.Token)),
		}
	}

	return err
}

// UpdateKubeConfig adds the devspace-cloud context if necessary and switches the current context
func UpdateKubeConfig(contextName, namespace string, cluster *api.Cluster, authInfo *api.AuthInfo, switchContext bool) error {
	config, err := kubeconfig.ReadKubeConfig(clientcmd.RecommendedHomeFile)
	if err != nil {
		return err
	}

	// Switch context if necessary
	if switchContext && config.CurrentContext != contextName {
		config.CurrentContext = contextName
	}

	config.Clusters[contextName] = cluster
	config.AuthInfos[contextName] = authInfo

	// Update kube context
	context := api.NewContext()
	context.Cluster = contextName
	context.AuthInfo = contextName
	context.Namespace = namespace

	config.Contexts[contextName] = context

	return kubeconfig.WriteKubeConfig(config, clientcmd.RecommendedHomeFile)
}

func startServer(redirectURI string, tokenChannel chan string) *http.Server {
	srv := &http.Server{Addr: ":25853"}

	http.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		keys, ok := r.URL.Query()["token"]
		if !ok || len(keys[0]) < 1 {
			log.Fatal("Bad request")
		}

		tokenChannel <- keys[0]
		http.Redirect(w, r, redirectURI, http.StatusSeeOther)
	})

	go func() {
		if err := srv.ListenAndServe(); err != nil {
			// cannot panic, because this probably is an intentional close
		}
	}()

	// returning reference so caller can call Shutdown()
	return srv
}
