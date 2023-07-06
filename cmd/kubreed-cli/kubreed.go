package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kubreed/pkg/libs"

	"github.com/rs/xid"
	flag "github.com/spf13/pflag"
	"gopkg.in/yaml.v2"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/client-go/util/retry"
)

// default values
// default values
const (
	Namespaces     = 1
	Namespace_name = ""
	Deployments    = 5
	Pods           = 3
	APIs           = 10
	RPS            = 1
	Branching      = 4
	Latency        = time.Second * 2
	PII            = 1
	ATTACK         = 1
	USER           = 1
)

var result []TrafficPattern

type TrafficPattern struct {
	Namespace string        `yaml:"namespace"`
	Pattern   []PatternItem `yaml:"pattern,flow"`
}

type PatternItem struct {
	Source      string   `yaml:"source"`
	Destination []string `yaml:"destination,flow"`
}

type appSpec struct {
	noOfPods  *int32
	branching *int
	image     *string
}

func GetTrafficPattern(clustername string) ([]TrafficPattern, error) {
	_, err := os.Stat("cmd/" + clustername + ".yaml")
	if os.IsNotExist(err) {
		return nil, nil
	}
	content, err := ioutil.ReadFile("cmd/" + clustername + ".yaml")
	if err != nil {
		return nil, err
	}
	err = yaml.Unmarshal(content, &result)
	if err != nil {
		return nil, err
	}

	return result, nil
}

func createDeploymentAndServices(clientset *kubernetes.Clientset, depName string, svcName string, namespace string,
	appConfig appSpec, depConfig libs.Config) error {
	ctx := context.Background()

	labels := map[string]string{
		"app": depName,
	}
	objectMeta := metav1.ObjectMeta{
		Name:      depName,
		Namespace: namespace,
		Labels:    labels,
	}

	depConfigBytes, err := json.Marshal(depConfig)
	if err != nil {
		log.Fatalf("Error creating Deployment Config: %v", err)
		return err
	}

	log.Printf("Creating Deployment: %q", depName)
	_, err = clientset.AppsV1().Deployments(namespace).Create(ctx,
		&appsv1.Deployment{
			ObjectMeta: objectMeta,
			Spec: appsv1.DeploymentSpec{
				Replicas: appConfig.noOfPods,
				Selector: &metav1.LabelSelector{
					MatchLabels: labels,
				},
				Template: v1.PodTemplateSpec{
					ObjectMeta: objectMeta,
					Spec: v1.PodSpec{
						Containers: []v1.Container{{
							Name:  "kubreed-http",
							Image: *appConfig.image,
							Ports: []v1.ContainerPort{{
								ContainerPort: 80,
								Protocol:      "TCP",
							}},
							Env: []v1.EnvVar{{
								Name:  libs.ConfigEnvVar,
								Value: string(depConfigBytes),
							}},
						}},
					},
				},
			},
		},
		metav1.CreateOptions{})
	if err != nil {
		log.Fatalf("Error creating deployment: %v", err)
		return err
	}
	log.Printf("Created deployment: %q", depName)

	log.Printf("Creating service: %q", svcName)
	_, err = clientset.CoreV1().Services(namespace).Create(ctx,
		&v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      svcName,
				Namespace: namespace,
				Labels:    map[string]string{},
			},
			Spec: v1.ServiceSpec{
				Selector: map[string]string{
					"app": depName,
				},
				Ports: []v1.ServicePort{
					{
						Name: "http-svc",
						Port: 80,
						TargetPort: intstr.IntOrString{
							Type:   intstr.Int,
							IntVal: 80,
						},
					},
				},
			},
		},
		metav1.CreateOptions{})
	if err != nil {
		log.Fatalf("Error creating service: %v", err)
		return err
	}
	return nil
}

func updateDeploymentAndServices(clientset *kubernetes.Clientset, depName string, svcName string, namespace string,
	appConfig appSpec, depConfig libs.Config) error {
	ctx := context.Background()

	depConfigBytes, err := json.Marshal(depConfig)
	if err != nil {
		log.Fatalf("Error creating Deployment Config: %v", err)
		return err
	}

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		result, getErr := clientset.AppsV1().Deployments(namespace).Get(ctx, depName, metav1.GetOptions{})
		if getErr != nil {
			panic(fmt.Errorf("failed to get latest version of deployment: %v", getErr))
		}

		result.Spec.Replicas = int32Ptr(*appConfig.noOfPods)
		result.Spec.Template.Spec.Containers[0].Image = *appConfig.image
		result.Spec.Template.Spec.Containers[0].Env = []v1.EnvVar{{
			Name:  libs.ConfigEnvVar,
			Value: string(depConfigBytes),
		}}
		_, updateErr := clientset.AppsV1().Deployments(namespace).Update(ctx, result, metav1.UpdateOptions{})
		return updateErr
	})
	if retryErr != nil {
		panic(fmt.Errorf("update failed: %v", retryErr))
	}
	fmt.Println("Updated deployment...")

	return nil
}

func createNamespace(clientset *kubernetes.Clientset) (string, error) {
	runID := xid.New()
	ctx := context.Background()
	namespace := runID.String()

	log.Printf("Creating namespace: %q", namespace)
	_, err := clientset.CoreV1().Namespaces().Create(ctx,
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
		metav1.CreateOptions{})
	if err != nil {
		return "", err
	}

	log.Printf("Created namespace: %q", namespace)
	nsstatus, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if nsstatus.Status.Phase != "Active" {
		return "", err
	}
	return namespace, err
}

func deleteDeploymentService(clientset *kubernetes.Clientset,
	deployment string, service string, namesapce string) error {
	ctx := context.Background()
	log.Printf("Deleting deployment: %q", deployment)
	deploymentsClient := clientset.AppsV1().Deployments(namesapce)
	err := deploymentsClient.Delete(ctx, deployment, metav1.DeleteOptions{})
	if err != nil {
		return err
	}
	log.Printf("Deleted Deployment: %q", deployment)
	log.Printf("Deleting Service: %q", service)
	ServiceClient := clientset.CoreV1().Services(namesapce)
	err = ServiceClient.Delete(ctx, service, metav1.DeleteOptions{})
	if err != nil {
		return err
	}
	log.Printf("Deleted Service: %q", service)
	return nil
}

func getRemoteServiceList(noofdep int, branching int, depname string) []string {
	var remoteServices []string
	dep_count, err := strconv.Atoi(depname[strings.LastIndex(depname, "-")+1:])
	if err != nil {
		log.Fatalf("not able to get the deployment prefix")
	}
	for k := 0; k < branching; k++ {
		for {
			remoteService := rand.Intn(noofdep)
			if ifServiceExist(fmt.Sprintf("app-%d", remoteService), remoteServices) {
				continue
			}
			if remoteService != dep_count {
				remoteServices = append(remoteServices,
					fmt.Sprintf("app-%d", remoteService))
				break
			}
			// loop until we get a remoteService that is not self
		}
	}
	return remoteServices
}

func removeIndex(str []PatternItem, index int) []PatternItem {
	return append(str[:index], str[index+1:]...)
}

func ifServiceExist(str string, list []string) bool {
	for _, data := range list {
		if data == str {
			return true
		}
	}
	return false
}

func int32Ptr(i int32) *int32 { return &i }

func main() {
	ns := flag.IntP("namespaces", "n", Namespaces, "Number of Namespaces to create")
	nsname := flag.StringP("namespace_name", "m", Namespace_name, "Name of the ns for update")
	deps := flag.IntP("deployments", "d", Deployments, "Number of Deployments/Services to create per Namespace")
	pods := flag.Int32P("pods", "p", Pods, "Number of Pods to create per Deployment")
	api := flag.IntP("apis", "a", APIs, "Number of APIs per Pod")
	piipercent := flag.IntP("piipercent", "h", PII, "percentage of PII Requests ")
	attackpercent := flag.IntP("attackpercent", "o", ATTACK, "percentage of attack Requests ")
	userpercent := flag.IntP("userpercent", "u", USER, "percentage of user Requests ")
	rps := flag.IntP("rps", "r", RPS, "Outgoing rps by each client Pod")
	branching := flag.IntP("branching", "b", Branching, "Number of Services to which each client Pod should make requests")
	latency := flag.DurationP("latency", "l", Latency, "Maximum response time in milliseconds for each API call")
	image := flag.StringP("image", "i", "psankar/kubreed-http:c5466e0", "Image for kubreed-http")

	var kubeconfig *string
	var value string

	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	flag.Parse()

	if *ns < 1 {
		log.Fatalf("Invalid number of Namespaces")
		return
	}

	if *pods < 1 {
		log.Fatalf("Atleast 1 Pod is needed per deployment")
		return
	}

	if *api < 1 {
		log.Fatalf("Atleast 1 API is needed per pod")
		return
	}

	if *piipercent < 1 || (*piipercent+*attackpercent+*userpercent) > 100 || *attackpercent < 1 || *userpercent < 1 {
		log.Fatalf("PII/Attack/User percentage should atleast be 1 and should not exceed 100")
		return
	}

	// Multiply the three values to see if either of them is zero
	if *rps**branching*int(*latency) == 0 {
		log.Fatalf("rps, branching, respTime should all be non-zero for traffic to happen")
		return
	}

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatalf("Reading kubeconfig failed: %v", err)
		return
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Connecting to kubernetes API server failed: %v", err)
		return
	}

	//get cluster name
	config1, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: *kubeconfig},
		&clientcmd.ConfigOverrides{
			CurrentContext: "",
		}).RawConfig()
	if err != nil {
		log.Fatalf("Error getting cluster name: %v", err)
		return
	}

	currentContext := strings.Split(config1.CurrentContext, "@")[0]
	clustername := currentContext[strings.LastIndex(currentContext, "/")+1:]

	appConfig := appSpec{
		noOfPods:  pods,
		branching: branching,
		image:     image,
	}

	depConfig := libs.Config{
		APICount:             *api,
		RPS:                  *rps,
		PiiPercent:           *piipercent,
		AttackPercent:        *attackpercent,
		UserPercent:          *userpercent,
		ResponseTimeInternal: latency.String(),
	}

	var count int
	result, err := GetTrafficPattern(clustername)
	if err != nil {
		log.Fatalf("Error getting yaml file: %v", err)
	}

	if *nsname != "" {
		log.Printf("updating deployment in given ns %s", *nsname)
		value = "update"
		count = len(result)
		if count == 0 {
			log.Fatalf("file is empty..pls create new deployment and service: %v", err)
			return
		}
	} else {
		value = "create"
	}

	switch value {
	case "create":
		for i := 0; i < *ns; i++ {
			var result1 TrafficPattern
			result1.Namespace, err = createNamespace(clientset)
			if err != nil {
				log.Fatalf("Error creating ns: %v", err)
			}
			for j := 0; j < *deps; j++ {
				svcName := fmt.Sprintf("app-%d", j)
				depName := fmt.Sprintf("app-%d", j)
				depConfig.RemoteServices = getRemoteServiceList(*deps, *appConfig.branching, depName)
				result1.Pattern = append(result1.Pattern, PatternItem{Source: depName, Destination: depConfig.RemoteServices})
				err = createDeploymentAndServices(clientset, depName, svcName, result1.Namespace, appConfig, depConfig)
				if err != nil {
					log.Fatalf("Error deploying app: %v", err)
				}
			}
			result = append(result, result1)
		}
	case "update":
		update := false
		for c := 0; c < count; c++ {
			if result[c].Namespace == *nsname {
				if len(result[c].Pattern) > *deps {
					for d := *deps; d <= len(result[c].Pattern); d++ {
						err = deleteDeploymentService(clientset, fmt.Sprintf("app-%d", d), fmt.Sprintf("app-%d", d), result[c].Namespace)
						if err != nil {
							log.Fatalf("Error deleting deploying/service: %v", err)
						}
						result[c].Pattern = removeIndex(result[c].Pattern, d-1)
					}
				}
				if len(result[c].Pattern) < *deps {

					for d := len(result[c].Pattern); d < *deps; d++ {
						svcName := fmt.Sprintf("app-%d", d)
						depName := fmt.Sprintf("app-%d", d)
						depConfig.RemoteServices = getRemoteServiceList(*deps, *appConfig.branching, depName)

						err := createDeploymentAndServices(clientset, depName, svcName, result[c].Namespace, appConfig, depConfig)
						if err != nil {
							log.Fatalf("Error deleting deploying/service: %v", err)
						}
						result[c].Pattern = append(result[c].Pattern, PatternItem{Source: depName, Destination: depConfig.RemoteServices})
					}
				}
				for j := 0; j < *deps; j++ {
					svcName := fmt.Sprintf("app-%d", j)
					depName := fmt.Sprintf("app-%d", j)
					depConfig.RemoteServices = getRemoteServiceList(*deps, *appConfig.branching, depName)
					err := updateDeploymentAndServices(clientset, depName, svcName, result[c].Namespace, appConfig, depConfig)
					if err != nil {
						log.Fatalf("Error updating deployment: %v", err)
					}
					result[c].Pattern[j] = PatternItem{Source: depName, Destination: depConfig.RemoteServices}
				}
				update = true
				break
			}
		}
		if !update {
			log.Fatalf("Namespace not found")
		}
	}

	trafficData, err := yaml.Marshal(&result)
	if err != nil {
		log.Fatalf("Error Marshalling yaml: %v", err)
	}

	fmt.Printf("trafficData is:\n*****\n%s\n*****\n", trafficData)
	err = ioutil.WriteFile("cmd/"+clustername+".yaml", trafficData, 0644)
	if err != nil {
		log.Fatalf("Error updating yaml file: %v", err)
	}
}
