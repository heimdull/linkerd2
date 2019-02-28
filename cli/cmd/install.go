package cmd

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"

	"github.com/golang/protobuf/jsonpb"
	"github.com/linkerd/linkerd2/cli/static"
	"github.com/linkerd/linkerd2/controller/gen/config"
	"github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/linkerd/linkerd2/pkg/tls"
	uuid "github.com/satori/go.uuid"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/renderutil"
	"k8s.io/helm/pkg/timeconv"
	"sigs.k8s.io/yaml"
)

type installConfig struct {
	Namespace                  string
	ControllerImage            string
	WebImage                   string
	PrometheusImage            string
	PrometheusVolumeName       string
	GrafanaImage               string
	GrafanaVolumeName          string
	ControllerReplicas         uint
	ImagePullPolicy            string
	UUID                       string
	CliVersion                 string
	ControllerLogLevel         string
	ControllerComponentLabel   string
	CreatedByAnnotation        string
	DestinationAPIPort         uint
	ProxyContainerName         string
	InboundPort                uint
	OutboundPort               uint
	IgnoreInboundPorts         string
	IgnoreOutboundPorts        string
	InboundAcceptKeepaliveMs   uint
	OutboundConnectKeepaliveMs uint
	ProxyAutoInjectEnabled     bool
	ProxyInjectAnnotation      string
	ProxyInjectDisabled        string
	ProxyLogLevel              string
	ProxyUID                   int64
	ProxyMetricsPort           uint
	ProxyControlPort           uint
	ProxySpecFileName          string
	ProxyInitSpecFileName      string
	ProxyInitImage             string
	ProxyImage                 string
	ProxyResourceRequestCPU    string
	ProxyResourceRequestMemory string
	ProxyResourceLimitCPU      string
	ProxyResourceLimitMemory   string
	SingleNamespace            bool
	EnableHA                   bool
	ControllerUID              int64
	ProfileSuffixes            string
	EnableH2Upgrade            bool
	NoInitContainer            bool
	GlobalConfig               string
	ProxyConfig                string

	Identity *installIdentityConfig
}

type installIdentityConfig struct {
	TrustDomain     string
	TrustAnchorsPEM string

	Issuer *issuerConfig
}

type issuerConfig struct {
	ClockSkewAllowance string
	IssuanceLifetime   string

	ExpiryAnnotation string
	Expiry           time.Time

	Key string
	Crt string
}

// installOptions holds values for command line flags that apply to the install
// command. All fields in this struct should have corresponding flags added in
// the newCmdInstall func later in this file. It also embeds proxyConfigOptions
// in order to hold values for command line flags that apply to both inject and
// install.
type installOptions struct {
	controllerReplicas uint
	controllerLogLevel string
	proxyAutoInject    bool
	singleNamespace    bool
	highAvailability   bool
	controllerUID      int64
	disableH2Upgrade   bool
	identityOptions    installIdentityOptions
	*proxyConfigOptions
}

type installIdentityOptions struct {
	trustDomain        string
	issuanceLifetime   time.Duration
	clockSkewAllowance time.Duration
}

const (
	prometheusProxyOutboundCapacity   = 10000
	defaultControllerReplicas         = 1
	defaultHAControllerReplicas       = 3
	defaultIdentityTrustDomain        = "cluster.local"
	defaultIdentityIssuanceLifetime   = 2 * time.Minute
	defaultIdentityClockSkewAllowance = 2 * time.Minute

	nsTemplateName             = "templates/namespace.yaml"
	identityTemplateName       = "templates/identity.yaml"
	controllerTemplateName     = "templates/controller.yaml"
	webTemplateName            = "templates/web.yaml"
	prometheusTemplateName     = "templates/prometheus.yaml"
	grafanaTemplateName        = "templates/grafana.yaml"
	serviceprofileTemplateName = "templates/serviceprofile.yaml"
	proxyInjectorTemplateName  = "templates/proxy_injector.yaml"
)

func newInstallOptions() *installOptions {
	return &installOptions{
		controllerReplicas: defaultControllerReplicas,
		controllerLogLevel: "info",
		proxyAutoInject:    false,
		singleNamespace:    false,
		highAvailability:   false,
		controllerUID:      2103,
		disableH2Upgrade:   false,
		proxyConfigOptions: newProxyConfigOptions(),
		identityOptions: installIdentityOptions{
			trustDomain:      defaultIdentityTrustDomain,
			issuanceLifetime: defaultIdentityIssuanceLifetime,
		},
	}
}

func newCmdInstall() *cobra.Command {
	options := newInstallOptions()

	cmd := &cobra.Command{
		Use:   "install [flags]",
		Short: "Output Kubernetes configs to install Linkerd",
		Long:  "Output Kubernetes configs to install Linkerd.",
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := validateAndBuildConfig(options)
			if err != nil {
				return err
			}

			return render(*config, os.Stdout, options)
		},
	}

	addProxyConfigFlags(cmd, options.proxyConfigOptions)
	cmd.PersistentFlags().UintVar(&options.controllerReplicas, "controller-replicas", options.controllerReplicas, "Replicas of the controller to deploy")
	cmd.PersistentFlags().StringVar(&options.controllerLogLevel, "controller-log-level", options.controllerLogLevel, "Log level for the controller and web components")
	cmd.PersistentFlags().BoolVar(&options.proxyAutoInject, "proxy-auto-inject", options.proxyAutoInject, "Enable proxy sidecar auto-injection via a webhook (default false)")
	cmd.PersistentFlags().BoolVar(&options.singleNamespace, "single-namespace", options.singleNamespace, "Experimental: Configure the control plane to only operate in the installed namespace (default false)")
	cmd.PersistentFlags().BoolVar(&options.highAvailability, "ha", options.highAvailability, "Experimental: Enable HA deployment config for the control plane (default false)")
	cmd.PersistentFlags().Int64Var(&options.controllerUID, "controller-uid", options.controllerUID, "Run the control plane components under this user ID")
	cmd.PersistentFlags().BoolVar(&options.disableH2Upgrade, "disable-h2-upgrade", options.disableH2Upgrade, "Prevents the controller from instructing proxies to perform transparent HTTP/2 upgrading (default false)")
	return cmd
}

func validateAndBuildConfig(options *installOptions) (*installConfig, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}

	// TODO: these seem to not be used?
	ignoreInboundPorts := []string{
		fmt.Sprintf("%d", options.proxyControlPort),
		fmt.Sprintf("%d", options.proxyMetricsPort),
	}
	for _, p := range options.ignoreInboundPorts {
		ignoreInboundPorts = append(ignoreInboundPorts, fmt.Sprintf("%d", p))
	}
	ignoreOutboundPorts := []string{}
	for _, p := range options.ignoreOutboundPorts {
		ignoreOutboundPorts = append(ignoreOutboundPorts, fmt.Sprintf("%d", p))
	}

	if options.highAvailability && options.controllerReplicas == defaultControllerReplicas {
		options.controllerReplicas = defaultHAControllerReplicas
	}

	if options.highAvailability && options.proxyCPURequest == "" {
		options.proxyCPURequest = "10m"
	}

	if options.highAvailability && options.proxyMemoryRequest == "" {
		options.proxyMemoryRequest = "20Mi"
	}

	profileSuffixes := "."
	if options.proxyConfigOptions.disableExternalProfiles {
		profileSuffixes = "svc.cluster.local."
	}

	var identity *installIdentityConfig
	trustDomain := options.identityOptions.trustDomain
	if options.identityOptions.trustDomain != "" {
		// TODO accept roots as configuration
		root, err := tls.GenerateRootCAWithDefaults(trustDomain)
		if err != nil {
			return nil, fmt.Errorf("Failed to create root certificate for identity: %s", err)
		}

		subdomain := fmt.Sprintf("identity.%s.%s", controlPlaneNamespace, trustDomain)
		issuer, err := root.GenerateCA(subdomain, tls.Validity{}, -1)
		if err != nil {
			return nil, fmt.Errorf("Failed to create issuer certificate for identity: %s", err)
		}

		// TODO preserve the root key (generate and display a password)

		identity = &installIdentityConfig{
			TrustDomain:     trustDomain,
			TrustAnchorsPEM: root.Cred.Crt.EncodeCertificatePEM(),
			Issuer: &issuerConfig{
				ClockSkewAllowance: options.identityOptions.clockSkewAllowance.String(),
				IssuanceLifetime:   options.identityOptions.issuanceLifetime.String(),
				Crt:                issuer.Cred.Crt.EncodeCertificatePEM(),
				Key:                issuer.Cred.EncodePrivateKeyPEM(),
				ExpiryAnnotation:   k8s.IdentityIssuerExpiryAnnotation,
				Expiry:             issuer.Cred.Crt.Certificate.NotAfter,
			},
		}
	}

	jsonMarshaler := jsonpb.Marshaler{EmitDefaults: true}
	globalConfig, err := jsonMarshaler.MarshalToString(globalConfig(options))
	if err != nil {
		return nil, err
	}

	proxyConfig, err := jsonMarshaler.MarshalToString(proxyConfig(options))
	if err != nil {
		return nil, err
	}

	return &installConfig{
		Namespace:                  controlPlaneNamespace,
		ControllerImage:            fmt.Sprintf("%s/controller:%s", options.dockerRegistry, options.linkerdVersion),
		WebImage:                   fmt.Sprintf("%s/web:%s", options.dockerRegistry, options.linkerdVersion),
		PrometheusImage:            "prom/prometheus:v2.7.1",
		PrometheusVolumeName:       "data",
		GrafanaImage:               fmt.Sprintf("%s/grafana:%s", options.dockerRegistry, options.linkerdVersion),
		GrafanaVolumeName:          "data",
		ControllerReplicas:         options.controllerReplicas,
		ImagePullPolicy:            options.imagePullPolicy,
		UUID:                       uuid.NewV4().String(),
		CliVersion:                 k8s.CreatedByAnnotationValue(),
		ControllerLogLevel:         options.controllerLogLevel,
		ControllerComponentLabel:   k8s.ControllerComponentLabel,
		ControllerUID:              options.controllerUID,
		CreatedByAnnotation:        k8s.CreatedByAnnotation,
		DestinationAPIPort:         options.destinationAPIPort,
		ProxyContainerName:         k8s.ProxyContainerName,
		InboundPort:                options.inboundPort,
		OutboundPort:               options.outboundPort,
		IgnoreInboundPorts:         strings.Join(ignoreInboundPorts, ","),
		IgnoreOutboundPorts:        strings.Join(ignoreOutboundPorts, ","),
		InboundAcceptKeepaliveMs:   defaultKeepaliveMs,
		OutboundConnectKeepaliveMs: defaultKeepaliveMs,
		ProxyAutoInjectEnabled:     options.proxyAutoInject,
		ProxyInjectAnnotation:      k8s.ProxyInjectAnnotation,
		ProxyInjectDisabled:        k8s.ProxyInjectDisabled,
		ProxyLogLevel:              options.proxyLogLevel,
		ProxyUID:                   options.proxyUID,
		ProxyMetricsPort:           options.proxyMetricsPort,
		ProxyControlPort:           options.proxyControlPort,
		ProxySpecFileName:          k8s.ProxySpecFileName,
		ProxyInitSpecFileName:      k8s.ProxyInitSpecFileName,
		ProxyInitImage:             options.taggedProxyInitImage(),
		ProxyImage:                 options.taggedProxyImage(),
		ProxyResourceRequestCPU:    options.proxyCPURequest,
		ProxyResourceRequestMemory: options.proxyMemoryRequest,
		ProxyResourceLimitCPU:      options.proxyCPULimit,
		ProxyResourceLimitMemory:   options.proxyMemoryLimit,
		SingleNamespace:            options.singleNamespace,
		EnableHA:                   options.highAvailability,
		ProfileSuffixes:            profileSuffixes,
		EnableH2Upgrade:            !options.disableH2Upgrade,
		NoInitContainer:            options.noInitContainer,
		Identity:                   identity,
		GlobalConfig:               globalConfig,
		ProxyConfig:                proxyConfig,
	}, nil
}

func render(config installConfig, w io.Writer, options *installOptions) error {
	// Render raw values and create chart config
	rawValues, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	chrtConfig := &chart.Config{Raw: string(rawValues), Values: map[string]*chart.Value{}}

	files := []*chartutil.BufferedFile{
		{Name: chartutil.ChartfileName},
		{Name: nsTemplateName},
		{Name: identityTemplateName},
		{Name: controllerTemplateName},
		{Name: serviceprofileTemplateName},
		{Name: webTemplateName},
		{Name: prometheusTemplateName},
		{Name: grafanaTemplateName},
		{Name: proxyInjectorTemplateName},
	}

	// Read templates into bytes
	for _, f := range files {
		data, err := readIntoBytes(f.Name)
		if err != nil {
			return err
		}
		f.Data = data
	}

	// Create chart and render templates
	chrt, err := chartutil.LoadFiles(files)
	if err != nil {
		return err
	}

	renderOpts := renderutil.Options{
		ReleaseOptions: chartutil.ReleaseOptions{
			Name:      "linkerd",
			IsInstall: true,
			IsUpgrade: false,
			Time:      timeconv.Now(),
			Namespace: controlPlaneNamespace,
		},
		KubeVersion: "",
	}

	renderedTemplates, err := renderutil.Render(chrt, chrtConfig, renderOpts)
	if err != nil {
		return err
	}

	// Merge templates and inject
	var buf bytes.Buffer
	for _, tmpl := range files {
		t := path.Join(renderOpts.ReleaseOptions.Name, tmpl.Name)
		if _, err := buf.WriteString(renderedTemplates[t]); err != nil {
			return err
		}
	}

	injectOptions := newInjectOptions()
	*injectOptions.proxyConfigOptions = *options.proxyConfigOptions

	// Special case for linkerd-proxy running in the Prometheus pod.
	injectOptions.proxyOutboundCapacity[config.PrometheusImage] = prometheusProxyOutboundCapacity

	// Skip outbound port 443 to enable Kubernetes API access without the proxy.
	// Once Kubernetes supports sidecar containers, this may be removed, as that
	// will guarantee the proxy is running prior to control-plane startup.
	injectOptions.ignoreOutboundPorts = append(injectOptions.ignoreOutboundPorts, 443)

	return InjectYAML(&buf, w, ioutil.Discard, injectOptions)
}

func (options *installOptions) validate() error {
	if _, err := log.ParseLevel(options.controllerLogLevel); err != nil {
		return fmt.Errorf("--controller-log-level must be one of: panic, fatal, error, warn, info, debug")
	}

	if options.proxyAutoInject && options.singleNamespace {
		return fmt.Errorf("The --proxy-auto-inject and --single-namespace flags cannot both be specified together")
	}

	return options.proxyConfigOptions.validate()
}

func readIntoBytes(filename string) ([]byte, error) {
	file, err := static.Templates.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	buf := new(bytes.Buffer)
	buf.ReadFrom(file)

	return buf.Bytes(), nil
}

func globalConfig(options *installOptions) *config.GlobalConfig {
	var identityContext *config.IdentityContext
	if options.identityOptions.trustDomain != "" {
		identityContext = new(config.IdentityContext)
	}

	return &config.GlobalConfig{
		LinkerdNamespace: controlPlaneNamespace,
		CniEnabled:       options.noInitContainer,
		IdentityContext:  identityContext,
	}
}

func proxyConfig(options *installOptions) *config.ProxyConfig {
	ignoreInboundPorts := []*config.Port{}
	for _, port := range options.ignoreInboundPorts {
		ignoreInboundPorts = append(ignoreInboundPorts, &config.Port{Port: uint32(port)})
	}

	ignoreOutboundPorts := []*config.Port{}
	for _, port := range options.ignoreOutboundPorts {
		ignoreOutboundPorts = append(ignoreOutboundPorts, &config.Port{Port: uint32(port)})
	}

	return &config.ProxyConfig{
		ProxyImage: &config.Image{
			ImageName:  options.taggedProxyImage(),
			PullPolicy: options.imagePullPolicy,
		},
		ProxyInitImage: &config.Image{
			ImageName:  options.taggedProxyInitImage(),
			PullPolicy: options.imagePullPolicy,
		},
		DestinationApiPort: &config.Port{
			Port: uint32(options.destinationAPIPort),
		},
		ControlPort: &config.Port{
			Port: uint32(options.proxyControlPort),
		},
		IgnoreInboundPorts:  ignoreInboundPorts,
		IgnoreOutboundPorts: ignoreOutboundPorts,
		InboundPort: &config.Port{
			Port: uint32(options.inboundPort),
		},
		MetricsPort: &config.Port{
			Port: uint32(options.proxyMetricsPort),
		},
		OutboundPort: &config.Port{
			Port: uint32(options.outboundPort),
		},
		Resource: &config.ResourceRequirements{
			RequestCpu:    options.proxyCPURequest,
			RequestMemory: options.proxyMemoryRequest,
			LimitCpu:      options.proxyCPULimit,
			LimitMemory:   options.proxyMemoryLimit,
		},
		ProxyUid: options.proxyUID,
		LogLevel: &config.LogLevel{
			Level: options.proxyLogLevel,
		},
		DisableExternalProfiles: options.disableExternalProfiles,
	}
}
