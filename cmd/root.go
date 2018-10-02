package cmd

import (
	"fmt"
	appsv1 "github.com/openshift/api/apps/v1"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/snowdrop/spring-boot-cloud-devex/pkg/buildpack"
	"github.com/snowdrop/spring-boot-cloud-devex/pkg/buildpack/types"
	"github.com/snowdrop/spring-boot-cloud-devex/pkg/common/config"
	"github.com/snowdrop/spring-boot-cloud-devex/pkg/common/oc"
	"github.com/spf13/cobra"
	"k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"path"
)

var (
	namespace string
	appName   string
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "sd",
	Short: "snowdrop's client tool",

	Long: `snowdrop's client tool to scaffold a spring boot application on kubernetes/openshift'`,

	Example: `    # Creating and deploying a spring Boot application
    git clone github.com/snowdrop/spring-boot-cloud-devex && cd spring-boot-cloud-devex/spring-boot
    sd init -n namespace
    sd push
    sd compile
    sd run`,
}

func init() {
	rootCmd.PersistentFlags().StringP("kubeconfig", "k", "", "Path to a kubeconfig ($HOME/.kube/config). Only required if out-of-cluster.")
	rootCmd.PersistentFlags().StringP("masterurl", "", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	// Global flag(s)
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "", "Namespace/project (defaults to current project)")
	rootCmd.PersistentFlags().StringVarP(&appName, "application", "a", "", "Application name (defaults to current directory name)")
	//rootCmd.MarkPersistentFlagRequired("namespace")
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		checkError(err, "Root execution")
	}
}

// checkError prints the cause of the given error and exits the code with an
// exit code of 1.
// If the context is provided, then that is printed, if not, then the cause is
// detected using errors.Cause(err)
func checkError(err error, context string, a ...interface{}) {
	if err != nil {
		log.Debugf("Error:\n%v", err)
		if context == "" {
			fmt.Println(errors.Cause(err))
		} else {
			fmt.Printf(fmt.Sprintf("%s\n", context), a...)
		}
		os.Exit(1)
	}
}

func Setup() config.Tool {
	tool := &config.Tool{}

	// Parse MANIFEST
	tool.Application = parseManifest()

	// Get K8s' config file
	tool.KubeConfig = getK8Config(*rootCmd)

	// Switch to namespace if specified or retrieve the current one if not
	currentNs, err := oc.ExecCommandAndReturn(oc.Command{Args: []string{"project", "-q", namespace}})
	if err != nil {
		log.Fatal(err)
	}
	log.Infof("Using '%s' namespace", currentNs)
	tool.Application.Namespace = currentNs

	// Create Kube Rest's Config Client
	tool.RestConfig = createKubeRestconfig(tool.KubeConfig)
	tool.Clientset = createClientSet(tool.KubeConfig, tool.RestConfig)

	finishSetupAndSetApplicationName(tool)

	return *tool
}

func SetupAndWaitForPod() (config.Tool, *v1.Pod) {
	setup := Setup()

	// Wait till the dev pod is available
	log.Info("Wait till the dev pod is available")
	pod, err := buildpack.WaitAndGetPod(setup.Clientset, setup.Application)
	if err != nil {
		log.Fatalf("Pod watch error: %s", err)
	}

	return setup, pod
}

func parseManifest() types.Application {
	log.Info("Parse MANIFEST of the project if it exists")
	current, _ := os.Getwd()
	return buildpack.ParseManifest(current + "/MANIFEST")
}

func getK8Config(cmd cobra.Command) config.Kube {
	log.Info("Get K8s config file")
	var kubeCfg = config.Kube{}
	kubeCfgPath := cmd.Flag("kubeconfig").Value.String()
	if kubeCfgPath == "" {
		kubeCfg.Config = config.HomeKubePath()
	} else {
		kubeCfg.Config = kubeCfgPath
	}
	log.Debug("Kubeconfig : ", kubeCfg)
	return kubeCfg
}

// Create Kube ClientSet
func createClientSet(kubeCfg config.Kube, optionalRestCfg ...*restclient.Config) *kubernetes.Clientset {
	var kubeRestClient *restclient.Config
	if len(optionalRestCfg) == 1 {
		kubeRestClient = optionalRestCfg[0]
	} else {
		kubeRestClient = createKubeRestconfig(kubeCfg)
	}
	log.Info("Create k8s Clientset")
	clientset, errclientset := kubernetes.NewForConfig(kubeRestClient)
	if errclientset != nil {
		log.Fatalf("Error building kubernetes clientset: %s", errclientset.Error())
	}
	return clientset
}

// Create Kube Rest's Config Client
func createKubeRestconfig(kubeCfg config.Kube) *restclient.Config {
	log.Info("Create k8s Rest config client using the developer's machine config file")
	kubeRestClient, err := clientcmd.BuildConfigFromFlags(kubeCfg.MasterURL, kubeCfg.Config)
	if err != nil {
		log.Fatalf("Error building kubeconfig: %s", err.Error())
	}
	return kubeRestClient
}

func finishSetupAndSetApplicationName(setup *config.Tool) {
	// check if we already have the DC set up, in which case use it for the name of the application
	existingDCs, err := oc.GetNamesByLabel("dc", buildpack.OdoLabelName, buildpack.OdoLabelValue)
	if err != nil {
		log.Fatalf("Error retrieving DeploymentConfig labeled %s=%s. Are you logged in?", buildpack.OdoLabelName, buildpack.OdoLabelValue)
	}
	if len(existingDCs) != 0 {
		//use the name of the first matching DeploymentConfig
		dcName := existingDCs[0]
		log.Infof("Using application name '%s' from the existing DeploymentConfig labeled with '%s=%s'", dcName, "io.openshift.odo", "inject-supervisord")
		setup.Application.Name = dcName
	} else {
		// otherwise, if no DeploymentConfig exists already, we need to set the development pod up
		log.Info("Setting up the development pod")

		// if we specified an application name via the invoked command, use it
		if len(appName) > 0 {
			log.Infof("Using explicit application name '%s'", appName)
			setup.Application.Name = appName
		} else if len(setup.Application.Name) > 0 {
			// if a value was already set in the Manifest, use it
			log.Infof("Using application name '%s' that was set in MANIFEST", setup.Application.Name)
		} else {
			// otherwise, use the name of the current directory as the application name
			current, _ := os.Getwd()
			directoryName := path.Base(current)
			log.Infof("Using (default) application name '%s' which is the name the project's directory", directoryName)
			setup.Application.Name = directoryName
		}

		// Create ImageStreams
		log.Info("Create ImageStreams for Supervisord and Java S2I Image of SpringBoot")
		buildpack.CreateDefaultImageStreams(setup.RestConfig, setup.Application)

		// Create PVC
		log.Info("Create PVC to store m2 repo")
		buildpack.CreatePVC(setup.Clientset, setup.Application, "1Gi")

		var dc *appsv1.DeploymentConfig
		log.Info("Create or retrieve DeploymentConfig using Supervisord and Java S2I Image of SpringBoot")
		dc = buildpack.CreateOrRetrieveDeploymentConfig(setup.RestConfig, setup.Application, "")

		log.Info("Create Service using Template")
		buildpack.CreateServiceTemplate(setup.Clientset, dc, setup.Application)

		log.Info("Create Route using Template")
		buildpack.CreateRouteTemplate(setup.RestConfig, setup.Application)
	}
}
