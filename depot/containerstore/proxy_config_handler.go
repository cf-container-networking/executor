package containerstore

import (
	"bytes"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/tedsuo/ifrit"
	yaml "gopkg.in/yaml.v2"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/executor"
	"code.cloudfoundry.org/executor/depot/containerstore/envoy"
	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/lager"
)

const (
	StartProxyPort = 61001
	EndProxyPort   = 65534

	TimeOut    = "0.25s"
	Static     = "STATIC"
	RoundRobin = "ROUND_ROBIN"

	IngressListener = "ingress_listener"
	TcpProxy        = "envoy.tcp_proxy"

	AdminAccessLog = "/dev/null"
)

var (
	ErrNoPortsAvailable   = errors.New("no ports available")
	ErrInvalidCertificate = errors.New("cannot parse invalid certificate")

	SupportedCipherSuites = "[ECDHE-RSA-AES256-GCM-SHA384|ECDHE-RSA-AES128-GCM-SHA256]"
)

var dummyRunner = func(credRotatedChan <-chan Credential) ifrit.Runner {
	return ifrit.RunFunc(func(signals <-chan os.Signal, ready chan<- struct{}) error {
		close(ready)
		for {
			select {
			case <-credRotatedChan:
			case <-signals:
				return nil
			}
		}
	})
}

type ProxyConfigHandler struct {
	logger                             lager.Logger
	containerProxyPath                 string
	containerProxyConfigPath           string
	containerProxyTrustedCACerts       []string
	containerProxyVerifySubjectAltName []string
	containerProxyRequireClientCerts   bool

	reloadDuration time.Duration
	reloadClock    clock.Clock
}

type NoopProxyConfigHandler struct{}

func (p *NoopProxyConfigHandler) CreateDir(logger lager.Logger, container executor.Container) ([]garden.BindMount, []executor.EnvironmentVariable, error) {
	return nil, nil, nil
}
func (p *NoopProxyConfigHandler) RemoveDir(logger lager.Logger, container executor.Container) error {
	return nil
}
func (p *NoopProxyConfigHandler) Update(credentials Credential, container executor.Container) error {
	return nil
}
func (p *NoopProxyConfigHandler) Close(invalidCredentials Credential, container executor.Container) error {
	return nil
}

func (p *NoopProxyConfigHandler) RemoveProxyConfigDir(logger lager.Logger, container executor.Container) error {
	return nil
}

func (p *NoopProxyConfigHandler) ProxyPorts(lager.Logger, *executor.Container) ([]executor.ProxyPortMapping, []uint16) {
	return nil, nil
}

func (p *NoopProxyConfigHandler) Runner(logger lager.Logger, container executor.Container, credRotatedChan <-chan Credential) (ifrit.Runner, error) {
	return dummyRunner(credRotatedChan), nil
}

func NewNoopProxyConfigHandler() *NoopProxyConfigHandler {
	return &NoopProxyConfigHandler{}
}

func NewProxyConfigHandler(
	logger lager.Logger,
	containerProxyPath string,
	containerProxyConfigPath string,
	ContainerProxyTrustedCACerts []string,
	ContainerProxyVerifySubjectAltName []string,
	containerProxyRequireClientCerts bool,
	reloadDuration time.Duration,
	reloadClock clock.Clock,
) *ProxyConfigHandler {
	return &ProxyConfigHandler{
		logger:                             logger.Session("proxy-manager"),
		containerProxyPath:                 containerProxyPath,
		containerProxyConfigPath:           containerProxyConfigPath,
		containerProxyTrustedCACerts:       ContainerProxyTrustedCACerts,
		containerProxyVerifySubjectAltName: ContainerProxyVerifySubjectAltName,
		containerProxyRequireClientCerts:   containerProxyRequireClientCerts,
		reloadDuration:                     reloadDuration,
		reloadClock:                        reloadClock,
	}
}

// This modifies the container pointer in order to create garden NetIn rules in the storenode.Create
func (p *ProxyConfigHandler) ProxyPorts(logger lager.Logger, container *executor.Container) ([]executor.ProxyPortMapping, []uint16) {
	if !container.EnableContainerProxy {
		return nil, nil
	}

	proxyPortMapping := []executor.ProxyPortMapping{}

	existingPorts := make(map[uint16]interface{})
	containerPorts := make([]uint16, len(container.Ports))
	for i, portMap := range container.Ports {
		existingPorts[portMap.ContainerPort] = struct{}{}
		containerPorts[i] = portMap.ContainerPort
	}

	extraPorts := []uint16{}

	portCount := 0
	for port := uint16(StartProxyPort); port < EndProxyPort; port++ {
		if portCount == len(existingPorts) {
			break
		}

		if existingPorts[port] != nil {
			continue
		}

		extraPorts = append(extraPorts, port)
		proxyPortMapping = append(proxyPortMapping, executor.ProxyPortMapping{
			AppPort:   containerPorts[portCount],
			ProxyPort: port,
		})

		portCount++
	}

	return proxyPortMapping, extraPorts
}

func (p *ProxyConfigHandler) CreateDir(logger lager.Logger, container executor.Container) ([]garden.BindMount, []executor.EnvironmentVariable, error) {
	if !container.EnableContainerProxy {
		return nil, nil, nil
	}

	logger.Info("adding-container-proxy-bindmounts")
	proxyConfigDir := filepath.Join(p.containerProxyConfigPath, container.Guid)
	mounts := []garden.BindMount{
		{
			Origin:  garden.BindMountOriginHost,
			SrcPath: p.containerProxyPath,
			DstPath: "/etc/cf-assets/envoy",
		},
		{
			Origin:  garden.BindMountOriginHost,
			SrcPath: proxyConfigDir,
			DstPath: "/etc/cf-assets/envoy_config",
		},
	}

	err := os.MkdirAll(proxyConfigDir, 0755)
	if err != nil {
		return nil, nil, err
	}

	return mounts, nil, nil
}

func (p *ProxyConfigHandler) RemoveDir(logger lager.Logger, container executor.Container) error {
	if !container.EnableContainerProxy {
		return nil
	}

	logger.Info("removing-container-proxy-config-dir")
	proxyConfigDir := filepath.Join(p.containerProxyConfigPath, container.Guid)
	return os.RemoveAll(proxyConfigDir)
}

func (p *ProxyConfigHandler) Update(credentials Credential, container executor.Container) error {
	if !container.EnableContainerProxy {
		return nil
	}

	return p.writeConfig(credentials, container)
}

func (p *ProxyConfigHandler) Close(invalidCredentials Credential, container executor.Container) error {
	if !container.EnableContainerProxy {
		return nil
	}

	err := p.writeConfig(invalidCredentials, container)
	if err != nil {
		return err
	}

	p.reloadClock.Sleep(p.reloadDuration)
	return nil
}

func (p *ProxyConfigHandler) writeConfig(credentials Credential, container executor.Container) error {
	proxyConfigPath := filepath.Join(p.containerProxyConfigPath, container.Guid, "envoy.yaml")
	sdsServerCertAndKeyPath := filepath.Join(p.containerProxyConfigPath, container.Guid, "sds-server-cert-and-key.yaml")
	sdsServerValidationContextPath := filepath.Join(p.containerProxyConfigPath, container.Guid, "sds-server-validation-context.yaml")

	adminPort, err := getAvailablePort(container.Ports)
	if err != nil {
		return err
	}

	proxyConfig := generateProxyConfig(container, adminPort, p.containerProxyRequireClientCerts)

	err = writeProxyConfig(proxyConfig, proxyConfigPath)
	if err != nil {
		return err
	}

	sdsServerCertAndKey := generateSDSCertificateResource(container, credentials)
	err = marshalAndWriteToFile(sdsServerCertAndKey, sdsServerCertAndKeyPath)
	if err != nil {
		return err
	}

	sdsServerValidationContext, err := generateSDSCAResource(container, credentials, p.containerProxyTrustedCACerts, p.containerProxyVerifySubjectAltName)
	if err != nil {
		return err
	}
	err = marshalAndWriteToFile(sdsServerValidationContext, sdsServerValidationContextPath)
	if err != nil {
		return err
	}

	return nil
}

func generateProxyConfig(container executor.Container, adminPort uint16, requireClientCerts bool) envoy.ProxyConfig {
	clusters := []envoy.Cluster{}
	for index, portMap := range container.Ports {
		clusterName := fmt.Sprintf("%d-service-cluster", index)
		clusters = append(clusters, envoy.Cluster{
			Name:              clusterName,
			ConnectionTimeout: TimeOut,
			Type:              Static,
			LbPolicy:          RoundRobin,
			Hosts: []envoy.Address{
				{SocketAddress: envoy.SocketAddress{Address: container.InternalIP, PortValue: portMap.ContainerPort}},
			},
			CircuitBreakers: envoy.CircuitBreakers{Thresholds: []envoy.Threshold{
				{MaxConnections: math.MaxUint32},
			}},
		})
	}

	config := envoy.ProxyConfig{
		Admin: envoy.Admin{
			AccessLogPath: AdminAccessLog,
			Address: envoy.Address{
				SocketAddress: envoy.SocketAddress{
					Address:   "127.0.0.1",
					PortValue: adminPort,
				},
			},
		},
		StaticResources: envoy.StaticResources{
			Clusters:  clusters,
			Listeners: generateListeners(container, requireClientCerts),
		},
	}
	return config
}

func writeProxyConfig(proxyConfig envoy.ProxyConfig, path string) error {
	data, err := yaml.Marshal(proxyConfig)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(path, data, 0666)
}

func marshalAndWriteToFile(toMarshal interface{}, path string) error {
	tmpPath := path + ".tmp"

	data, err := yaml.Marshal(toMarshal)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(tmpPath, data, 0666)
	if err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func generateListeners(container executor.Container, requireClientCerts bool) []envoy.Listener {
	listeners := []envoy.Listener{}

	for index, portMap := range container.Ports {
		listenerName := TcpProxy
		clusterName := fmt.Sprintf("%d-service-cluster", index)

		listener := envoy.Listener{
			Name:    fmt.Sprintf("listener-%d", portMap.ContainerPort),
			Address: envoy.Address{SocketAddress: envoy.SocketAddress{Address: "0.0.0.0", PortValue: portMap.ContainerTLSProxyPort}},
			FilterChains: []envoy.FilterChain{envoy.FilterChain{
				Filters: []envoy.Filter{
					envoy.Filter{
						Name: listenerName,
						Config: envoy.Config{
							StatPrefix: fmt.Sprintf("%d-stats", index),
							Cluster:    clusterName,
						},
					},
				},
				TLSContext: envoy.TLSContext{
					RequireClientCertificate: requireClientCerts,
					CommonTLSContext: envoy.CommonTLSContext{
						TLSCertificateSDSSecretConfigs: envoy.SecretConfig{
							Name:      "server-cert-and-key",
							SDSConfig: envoy.SDSConfig{Path: "/etc/cf-assets/envoy_config/sds-server-cert-and-key.yaml"},
						},
						TLSParams: envoy.TLSParams{
							CipherSuites: SupportedCipherSuites,
						},
					},
				},
			},
			},
		}

		if requireClientCerts {
			listener.FilterChains[0].TLSContext.CommonTLSContext.ValidationContextSDSSecretConfig = envoy.SecretConfig{
				Name:      "server-validation-context",
				SDSConfig: envoy.SDSConfig{Path: "/etc/cf-assets/envoy_config/sds-server-validation-context.yaml"},
			}
		}

		listeners = append(listeners, listener)
	}

	return listeners
}

func generateSDSCertificateResource(container executor.Container, creds Credential) envoy.SDSCertificateResource {
	resources := []envoy.CertificateResource{{
		Type: "type.googleapis.com/envoy.api.v2.auth.Secret",
		Name: "server-cert-and-key",
		TLSCertificate: envoy.TLSCertificate{
			CertificateChain: envoy.DataSource{InlineString: creds.Cert},
			PrivateKey:       envoy.DataSource{InlineString: creds.Key},
		},
	}}

	return envoy.SDSCertificateResource{VersionInfo: "0", Resources: resources}
}

func generateSDSCAResource(container executor.Container, creds Credential, trustedCaCerts []string, subjectAltNames []string) (envoy.SDSCAResource, error) {
	certs, err := pemConcatenate(trustedCaCerts)
	if err != nil {
		return envoy.SDSCAResource{}, err
	}

	resources := []envoy.CAResource{{
		Type: "type.googleapis.com/envoy.api.v2.auth.Secret",
		Name: "server-validation-context",
		ValidationContext: envoy.CertificateValidationContext{
			TrustedCA:            envoy.DataSource{InlineString: certs},
			VerifySubjectAltName: subjectAltNames,
		},
	}}

	return envoy.SDSCAResource{VersionInfo: "0", Resources: resources}, nil
}

func pemConcatenate(certs []string) (string, error) {
	var certificateBuf bytes.Buffer
	for _, cert := range certs {
		block, _ := pem.Decode([]byte(cert))
		if block == nil {
			return "", errors.New("failed to read certificate.")
		}
		pem.Encode(&certificateBuf, block)
	}
	return certificateBuf.String(), nil
}

func getAvailablePort(allocatedPorts []executor.PortMapping, extraKnownPorts ...uint16) (uint16, error) {
	existingPorts := make(map[uint16]interface{})
	for _, portMap := range allocatedPorts {
		existingPorts[portMap.ContainerPort] = struct{}{}
		existingPorts[portMap.ContainerTLSProxyPort] = struct{}{}
	}

	for _, extraKnownPort := range extraKnownPorts {
		existingPorts[extraKnownPort] = struct{}{}
	}

	for port := uint16(StartProxyPort); port < EndProxyPort; port++ {
		if existingPorts[port] != nil {
			continue
		}
		return port, nil
	}
	return 0, ErrNoPortsAvailable
}
