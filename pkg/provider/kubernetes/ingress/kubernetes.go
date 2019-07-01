package ingress

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/containous/traefik/pkg/config"
	"github.com/containous/traefik/pkg/job"
	"github.com/containous/traefik/pkg/log"
	"github.com/containous/traefik/pkg/safe"
	"github.com/containous/traefik/pkg/tls"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	annotationKubernetesIngressClass = "kubernetes.io/ingress.class"
	traefikDefaultIngressClass       = "traefik"
)

// Provider holds configurations of the provider.
type Provider struct {
	Endpoint               string           `description:"Kubernetes server endpoint (required for external cluster client)." json:"endpoint,omitempty" toml:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Token                  string           `description:"Kubernetes bearer token (not needed for in-cluster client)." json:"token,omitempty" toml:"token,omitempty" yaml:"token,omitempty"`
	CertAuthFilePath       string           `description:"Kubernetes certificate authority file path (not needed for in-cluster client)." json:"certAuthFilePath,omitempty" toml:"certAuthFilePath,omitempty" yaml:"certAuthFilePath,omitempty"`
	DisablePassHostHeaders bool             `description:"Kubernetes disable PassHost Headers." json:"disablePassHostHeaders,omitempty" toml:"disablePassHostHeaders,omitempty" yaml:"disablePassHostHeaders,omitempty" export:"true"`
	Namespaces             []string         `description:"Kubernetes namespaces." json:"namespaces,omitempty" toml:"namespaces,omitempty" yaml:"namespaces,omitempty" export:"true"`
	LabelSelector          string           `description:"Kubernetes Ingress label selector to use." json:"labelSelector,omitempty" toml:"labelSelector,omitempty" yaml:"labelSelector,omitempty" export:"true"`
	IngressClass           string           `description:"Value of kubernetes.io/ingress.class annotation to watch for." json:"ingressClass,omitempty" toml:"ingressClass,omitempty" yaml:"ingressClass,omitempty" export:"true"`
	IngressEndpoint        *EndpointIngress `description:"Kubernetes Ingress Endpoint." json:"ingressEndpoint,omitempty" toml:"ingressEndpoint,omitempty" yaml:"ingressEndpoint,omitempty"`
	lastConfiguration      safe.Safe
}

// EndpointIngress holds the endpoint information for the Kubernetes provider
type EndpointIngress struct {
	IP               string `description:"IP used for Kubernetes Ingress endpoints." json:"ip,omitempty" toml:"ip,omitempty" yaml:"ip,omitempty"`
	Hostname         string `description:"Hostname used for Kubernetes Ingress endpoints." json:"hostname,omitempty" toml:"hostname,omitempty" yaml:"hostname,omitempty"`
	PublishedService string `description:"Published Kubernetes Service to copy status from." json:"publishedService,omitempty" toml:"publishedService,omitempty" yaml:"publishedService,omitempty"`
}

func (p *Provider) newK8sClient(ctx context.Context, ingressLabelSelector string) (*clientWrapper, error) {
	ingLabelSel, err := labels.Parse(ingressLabelSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid ingress label selector: %q", ingressLabelSelector)
	}

	logger := log.FromContext(ctx)

	logger.Infof("ingress label selector is: %q", ingLabelSel)

	withEndpoint := ""
	if p.Endpoint != "" {
		withEndpoint = fmt.Sprintf(" with endpoint %v", p.Endpoint)
	}

	var cl *clientWrapper
	switch {
	case os.Getenv("KUBERNETES_SERVICE_HOST") != "" && os.Getenv("KUBERNETES_SERVICE_PORT") != "":
		logger.Infof("Creating in-cluster Provider client%s", withEndpoint)
		cl, err = newInClusterClient(p.Endpoint)
	case os.Getenv("KUBECONFIG") != "":
		logger.Infof("Creating cluster-external Provider client from KUBECONFIG %s", os.Getenv("KUBECONFIG"))
		cl, err = newExternalClusterClientFromFile(os.Getenv("KUBECONFIG"))
	default:
		logger.Infof("Creating cluster-external Provider client%s", withEndpoint)
		cl, err = newExternalClusterClient(p.Endpoint, p.Token, p.CertAuthFilePath)
	}

	if err == nil {
		cl.ingressLabelSelector = ingLabelSel
	}

	return cl, err
}

// Init the provider.
func (p *Provider) Init() error {
	return nil
}

// Provide allows the k8s provider to provide configurations to traefik
// using the given configuration channel.
func (p *Provider) Provide(configurationChan chan<- config.Message, pool *safe.Pool) error {
	ctxLog := log.With(context.Background(), log.Str(log.ProviderName, "kubernetes"))
	logger := log.FromContext(ctxLog)
	// Tell glog (used by client-go) to log into STDERR. Otherwise, we risk
	// certain kinds of API errors getting logged into a directory not
	// available in a `FROM scratch` Docker container, causing glog to abort
	// hard with an exit code > 0.
	err := flag.Set("logtostderr", "true")
	if err != nil {
		return err
	}

	logger.Debugf("Using Ingress label selector: %q", p.LabelSelector)
	k8sClient, err := p.newK8sClient(ctxLog, p.LabelSelector)
	if err != nil {
		return err
	}

	pool.Go(func(stop chan bool) {
		operation := func() error {
			stopWatch := make(chan struct{}, 1)
			defer close(stopWatch)

			eventsChan, err := k8sClient.WatchAll(p.Namespaces, stopWatch)
			if err != nil {
				logger.Errorf("Error watching kubernetes events: %v", err)
				timer := time.NewTimer(1 * time.Second)
				select {
				case <-timer.C:
					return err
				case <-stop:
					return nil
				}
			}

			for {
				select {
				case <-stop:
					return nil
				case event := <-eventsChan:
					conf := p.loadConfigurationFromIngresses(ctxLog, k8sClient)

					if reflect.DeepEqual(p.lastConfiguration.Get(), conf) {
						logger.Debugf("Skipping Kubernetes event kind %T", event)
					} else {
						p.lastConfiguration.Set(conf)
						configurationChan <- config.Message{
							ProviderName:  "kubernetes",
							Configuration: conf,
						}
					}
				}
			}
		}

		notify := func(err error, time time.Duration) {
			logger.Errorf("Provider connection error: %s; retrying in %s", err, time)
		}
		err := backoff.RetryNotify(safe.OperationWithRecover(operation), job.NewBackOff(backoff.NewExponentialBackOff()), notify)
		if err != nil {
			logger.Errorf("Cannot connect to Provider: %s", err)
		}
	})

	return nil
}

func checkStringQuoteValidity(value string) error {
	_, err := strconv.Unquote(`"` + value + `"`)
	return err
}

func loadService(client Client, namespace string, backend v1beta1.IngressBackend) (*config.Service, error) {
	service, exists, err := client.GetService(namespace, backend.ServiceName)
	if err != nil {
		return nil, err
	}

	if !exists {
		return nil, errors.New("service not found")
	}

	var servers []config.Server
	var portName string
	var portSpec corev1.ServicePort
	var match bool
	for _, p := range service.Spec.Ports {
		if (backend.ServicePort.Type == intstr.Int && backend.ServicePort.IntVal == p.Port) ||
			(backend.ServicePort.Type == intstr.String && backend.ServicePort.StrVal == p.Name) {
			portName = p.Name
			portSpec = p
			match = true
			break
		}
	}

	if !match {
		return nil, errors.New("service port not found")
	}

	if service.Spec.Type == corev1.ServiceTypeExternalName {
		servers = append(servers, config.Server{
			URL: fmt.Sprintf("http://%s:%d", service.Spec.ExternalName, portSpec.Port),
		})
	} else {
		endpoints, endpointsExists, endpointsErr := client.GetEndpoints(namespace, backend.ServiceName)
		if endpointsErr != nil {
			return nil, endpointsErr
		}

		if !endpointsExists {
			return nil, errors.New("endpoints not found")
		}

		if len(endpoints.Subsets) == 0 {
			return nil, errors.New("subset not found")
		}

		var port int32
		for _, subset := range endpoints.Subsets {

			for _, p := range subset.Ports {
				if portName == p.Name {
					port = p.Port
					break
				}
			}

			if port == 0 {
				return nil, errors.New("cannot define a port")
			}

			protocol := "http"
			if port == 443 || strings.HasPrefix(portName, "https") {
				protocol = "https"
			}

			for _, addr := range subset.Addresses {
				servers = append(servers, config.Server{
					URL: fmt.Sprintf("%s://%s:%d", protocol, addr.IP, port),
				})
			}
		}
	}

	return &config.Service{
		LoadBalancer: &config.LoadBalancerService{
			Servers:        servers,
			PassHostHeader: true,
		},
	}, nil
}

func (p *Provider) loadConfigurationFromIngresses(ctx context.Context, client Client) *config.Configuration {
	conf := &config.Configuration{
		HTTP: &config.HTTPConfiguration{
			Routers:     map[string]*config.Router{},
			Middlewares: map[string]*config.Middleware{},
			Services:    map[string]*config.Service{},
		},
		TCP: &config.TCPConfiguration{},
	}

	ingresses := client.GetIngresses()

	tlsConfigs := make(map[string]*tls.CertAndStores)
	for _, ingress := range ingresses {
		ctx = log.With(ctx, log.Str("ingress", ingress.Name), log.Str("namespace", ingress.Namespace))

		if !shouldProcessIngress(p.IngressClass, ingress.Annotations[annotationKubernetesIngressClass]) {
			continue
		}

		err := getTLS(ctx, ingress, client, tlsConfigs)
		if err != nil {
			log.FromContext(ctx).Errorf("Error configuring TLS: %v", err)
		}

		if len(ingress.Spec.Rules) == 0 {
			if ingress.Spec.Backend != nil {
				if _, ok := conf.HTTP.Services["default-backend"]; ok {
					log.FromContext(ctx).Error("The default backend already exists.")
					continue
				}

				service, err := loadService(client, ingress.Namespace, *ingress.Spec.Backend)
				if err != nil {
					log.FromContext(ctx).
						WithField("serviceName", ingress.Spec.Backend.ServiceName).
						WithField("servicePort", ingress.Spec.Backend.ServicePort.String()).
						Errorf("Cannot create service: %v", err)
					continue
				}

				conf.HTTP.Routers["/"] = &config.Router{
					Rule:     "PathPrefix(`/`)",
					Priority: math.MinInt32,
					Service:  "default-backend",
				}

				conf.HTTP.Services["default-backend"] = service
			}
		}
		for _, rule := range ingress.Spec.Rules {
			if err := checkStringQuoteValidity(rule.Host); err != nil {
				log.FromContext(ctx).Errorf("Invalid syntax for host: %s", rule.Host)
				continue
			}

			for _, p := range rule.HTTP.Paths {
				service, err := loadService(client, ingress.Namespace, p.Backend)
				if err != nil {
					log.FromContext(ctx).
						WithField("serviceName", p.Backend.ServiceName).
						WithField("servicePort", p.Backend.ServicePort.String()).
						Errorf("Cannot create service: %v", err)
					continue
				}

				if err = checkStringQuoteValidity(p.Path); err != nil {
					log.FromContext(ctx).Errorf("Invalid syntax for path: %s", p.Path)
					continue
				}

				serviceName := ingress.Namespace + "/" + p.Backend.ServiceName + "/" + p.Backend.ServicePort.String()
				serviceName = strings.ReplaceAll(serviceName, ".", "-")
				var rules []string
				if len(rule.Host) > 0 {
					rules = []string{"Host(`" + rule.Host + "`)"}
				}

				if len(p.Path) > 0 {
					rules = append(rules, "PathPrefix(`"+p.Path+"`)")
				}

				conf.HTTP.Routers[strings.Replace(rule.Host, ".", "-", -1)+p.Path] = &config.Router{
					Rule:    strings.Join(rules, " && "),
					Service: serviceName,
				}

				conf.HTTP.Services[serviceName] = service
			}
			err := p.updateIngressStatus(ingress, client)
			if err != nil {
				log.FromContext(ctx).Errorf("Error while updating ingress status: %v", err)
			}
		}
	}

	certs := getTLSConfig(tlsConfigs)
	if len(certs) > 0 {
		conf.TLS = &config.TLSConfiguration{
			Certificates: certs,
		}
	}

	return conf
}

func shouldProcessIngress(ingressClass string, ingressClassAnnotation string) bool {
	return ingressClass == ingressClassAnnotation ||
		(len(ingressClass) == 0 && ingressClassAnnotation == traefikDefaultIngressClass)
}

func getTLS(ctx context.Context, ingress *v1beta1.Ingress, k8sClient Client, tlsConfigs map[string]*tls.CertAndStores) error {
	for _, t := range ingress.Spec.TLS {
		if t.SecretName == "" {
			log.FromContext(ctx).Debugf("Skipping TLS sub-section: No secret name provided")
			continue
		}

		configKey := ingress.Namespace + "/" + t.SecretName
		if _, tlsExists := tlsConfigs[configKey]; !tlsExists {
			secret, exists, err := k8sClient.GetSecret(ingress.Namespace, t.SecretName)
			if err != nil {
				return fmt.Errorf("failed to fetch secret %s/%s: %v", ingress.Namespace, t.SecretName, err)
			}
			if !exists {
				return fmt.Errorf("secret %s/%s does not exist", ingress.Namespace, t.SecretName)
			}

			cert, key, err := getCertificateBlocks(secret, ingress.Namespace, t.SecretName)
			if err != nil {
				return err
			}

			tlsConfigs[configKey] = &tls.CertAndStores{
				Certificate: tls.Certificate{
					CertFile: tls.FileOrContent(cert),
					KeyFile:  tls.FileOrContent(key),
				},
			}
		}
	}

	return nil
}

func getTLSConfig(tlsConfigs map[string]*tls.CertAndStores) []*tls.CertAndStores {
	var secretNames []string
	for secretName := range tlsConfigs {
		secretNames = append(secretNames, secretName)
	}
	sort.Strings(secretNames)

	var configs []*tls.CertAndStores
	for _, secretName := range secretNames {
		configs = append(configs, tlsConfigs[secretName])
	}

	return configs
}

func getCertificateBlocks(secret *corev1.Secret, namespace, secretName string) (string, string, error) {
	var missingEntries []string

	tlsCrtData, tlsCrtExists := secret.Data["tls.crt"]
	if !tlsCrtExists {
		missingEntries = append(missingEntries, "tls.crt")
	}

	tlsKeyData, tlsKeyExists := secret.Data["tls.key"]
	if !tlsKeyExists {
		missingEntries = append(missingEntries, "tls.key")
	}

	if len(missingEntries) > 0 {
		return "", "", fmt.Errorf("secret %s/%s is missing the following TLS data entries: %s",
			namespace, secretName, strings.Join(missingEntries, ", "))
	}

	cert := string(tlsCrtData)
	if cert == "" {
		missingEntries = append(missingEntries, "tls.crt")
	}

	key := string(tlsKeyData)
	if key == "" {
		missingEntries = append(missingEntries, "tls.key")
	}

	if len(missingEntries) > 0 {
		return "", "", fmt.Errorf("secret %s/%s contains the following empty TLS data entries: %s",
			namespace, secretName, strings.Join(missingEntries, ", "))
	}

	return cert, key, nil
}

func (p *Provider) updateIngressStatus(i *v1beta1.Ingress, k8sClient Client) error {
	// Only process if an EndpointIngress has been configured
	if p.IngressEndpoint == nil {
		return nil
	}

	if len(p.IngressEndpoint.PublishedService) == 0 {
		if len(p.IngressEndpoint.IP) == 0 && len(p.IngressEndpoint.Hostname) == 0 {
			return errors.New("publishedService or ip or hostname must be defined")
		}

		return k8sClient.UpdateIngressStatus(i.Namespace, i.Name, p.IngressEndpoint.IP, p.IngressEndpoint.Hostname)
	}

	serviceInfo := strings.Split(p.IngressEndpoint.PublishedService, "/")
	if len(serviceInfo) != 2 {
		return fmt.Errorf("invalid publishedService format (expected 'namespace/service' format): %s", p.IngressEndpoint.PublishedService)
	}
	serviceNamespace, serviceName := serviceInfo[0], serviceInfo[1]

	service, exists, err := k8sClient.GetService(serviceNamespace, serviceName)
	if err != nil {
		return fmt.Errorf("cannot get service %s, received error: %s", p.IngressEndpoint.PublishedService, err)
	}

	if exists && service.Status.LoadBalancer.Ingress == nil {
		// service exists, but has no Load Balancer status
		log.Debugf("Skipping updating Ingress %s/%s due to service %s having no status set", i.Namespace, i.Name, p.IngressEndpoint.PublishedService)
		return nil
	}

	if !exists {
		return fmt.Errorf("missing service: %s", p.IngressEndpoint.PublishedService)
	}

	return k8sClient.UpdateIngressStatus(i.Namespace, i.Name, service.Status.LoadBalancer.Ingress[0].IP, service.Status.LoadBalancer.Ingress[0].Hostname)
}
