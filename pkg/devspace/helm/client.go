package helm

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/covexo/devspace/pkg/util/fsutil"
	"github.com/covexo/devspace/pkg/util/log"

	"k8s.io/helm/pkg/getter"
	"k8s.io/helm/pkg/kube"
	"k8s.io/helm/pkg/repo"

	"k8s.io/client-go/kubernetes"

	"github.com/covexo/devspace/pkg/devspace/config/configutil"
	"github.com/covexo/devspace/pkg/devspace/kubectl"
	homedir "github.com/mitchellh/go-homedir"
	k8shelm "k8s.io/helm/pkg/helm"
	helmenvironment "k8s.io/helm/pkg/helm/environment"
	"k8s.io/helm/pkg/helm/helmpath"
	"k8s.io/helm/pkg/helm/portforwarder"
	rls "k8s.io/helm/pkg/proto/hapi/services"
	helmstoragedriver "k8s.io/helm/pkg/storage/driver"
)

// Get Client only once
var helmClient *ClientWrapper
var getOnce sync.Once

// ClientWrapper holds the necessary information for helm
type ClientWrapper struct {
	Client    *k8shelm.Client
	Settings  *helmenvironment.EnvSettings
	Namespace string
	kubectl   *kubernetes.Clientset
}

// NewClient creates a new helm client
func NewClient(kubectlClient *kubernetes.Clientset, log log.Logger, upgradeTiller bool) (*ClientWrapper, error) {
	var outerError error

	getOnce.Do(func() {
		config := configutil.GetConfig()
		if config.Tiller == nil || config.Tiller.Namespace == nil {
			outerError = errors.New("No tiller namespace specified")
			return
		}

		tillerNamespace := *config.Tiller.Namespace
		kubeconfig, err := kubectl.GetClientConfig()
		if err != nil {
			outerError = err
			return
		}

		err = ensureTiller(kubectlClient, config, upgradeTiller)
		if err != nil {
			outerError = err
			return
		}

		var tunnel *kube.Tunnel

		tunnelWaitTime := 2 * 60 * time.Second
		tunnelCheckInterval := 5 * time.Second

		log.StartWait("Waiting for " + tillerNamespace + "/tiller-deploy to become ready")
		defer log.StopWait()

		// Next we wait till we can establish a tunnel to the running pod
		for true {
			tunnel, err = portforwarder.New(tillerNamespace, kubectlClient, kubeconfig)
			if err == nil && tunnel != nil {
				break
			}

			if tunnelWaitTime <= 0 {
				outerError = err
				return
			}

			tunnelWaitTime = tunnelWaitTime - tunnelCheckInterval
			time.Sleep(tunnelCheckInterval)
		}

		helmWaitTime := 2 * 60 * time.Second
		helmCheckInterval := 5 * time.Second

		helmOptions := []k8shelm.Option{
			k8shelm.Host("127.0.0.1:" + strconv.Itoa(tunnel.Local)),
			k8shelm.ConnectTimeout(int64(helmCheckInterval)),
		}

		client := k8shelm.NewClient(helmOptions...)
		var tillerError error

		for helmWaitTime > 0 {
			_, tillerError = client.ListReleases(k8shelm.ReleaseListLimit(1))
			if tillerError == nil || helmWaitTime < 0 {
				break
			}

			helmWaitTime = helmWaitTime - helmCheckInterval
			time.Sleep(helmCheckInterval)
		}

		log.StopWait()

		if tillerError != nil {
			outerError = tillerError
			return
		}

		homeDir, err := homedir.Dir()
		if err != nil {
			outerError = err
			return
		}

		helmHomePath := homeDir + "/.devspace/helm"
		repoPath := helmHomePath + "/repository"
		repoFile := repoPath + "/repositories.yaml"
		stableRepoCachePathAbs := helmHomePath + "/" + stableRepoCachePath

		os.MkdirAll(helmHomePath+"/cache", os.ModePerm)
		os.MkdirAll(repoPath, os.ModePerm)
		os.MkdirAll(filepath.Dir(stableRepoCachePathAbs), os.ModePerm)

		_, repoFileNotFound := os.Stat(repoFile)
		if repoFileNotFound != nil {
			err = fsutil.WriteToFile([]byte(defaultRepositories), repoFile)
			if err != nil {
				outerError = err
				return
			}
		}

		wrapper := &ClientWrapper{
			Client: client,
			Settings: &helmenvironment.EnvSettings{
				Home: helmpath.Home(helmHomePath),
			},
			Namespace: tillerNamespace,
			kubectl:   kubectlClient,
		}

		_, err = os.Stat(stableRepoCachePathAbs)
		if err != nil {
			err = wrapper.updateRepos()
			if err != nil {
				outerError = err
				return
			}
		}

		helmClient = wrapper
	})

	return helmClient, outerError
}

func (helmClientWrapper *ClientWrapper) updateRepos() error {
	allRepos, err := repo.LoadRepositoriesFile(helmClientWrapper.Settings.Home.RepositoryFile())
	if err != nil {
		return err
	}

	repos := []*repo.ChartRepository{}

	for _, repoData := range allRepos.Repositories {
		repo, err := repo.NewChartRepository(repoData, getter.All(*helmClientWrapper.Settings))
		if err != nil {
			return err
		}

		repos = append(repos, repo)
	}

	wg := sync.WaitGroup{}

	for _, re := range repos {
		wg.Add(1)

		go func(re *repo.ChartRepository) {
			defer wg.Done()

			err := re.DownloadIndexFile(helmClientWrapper.Settings.Home.String())
			if err != nil {
				log.With(err).Error("Unable to download repo index")

				//TODO
			}
		}(re)
	}

	wg.Wait()

	return nil
}

// ReleaseExists checks if the given release name exists
func (helmClientWrapper *ClientWrapper) ReleaseExists(releaseName string) (bool, error) {
	_, err := helmClientWrapper.Client.ReleaseHistory(releaseName, k8shelm.WithMaxHistory(1))
	if err != nil {
		if strings.Contains(err.Error(), helmstoragedriver.ErrReleaseNotFound(releaseName).Error()) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

// DeleteRelease deletes a helm release and optionally purges it
func (helmClientWrapper *ClientWrapper) DeleteRelease(releaseName string, purge bool) (*rls.UninstallReleaseResponse, error) {
	return helmClientWrapper.Client.DeleteRelease(releaseName, k8shelm.DeletePurge(purge))
}
