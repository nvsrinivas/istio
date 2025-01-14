// Copyright 2019 Istio Authors
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

package bootstrap

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"

	"istio.io/istio/security/pkg/k8s/chiron"
	"istio.io/pkg/log"
)

const (
	// defaultCertGracePeriodRatio is the default length of certificate rotation grace period,
	// configured as the ratio of the certificate TTL.
	defaultCertGracePeriodRatio = 0.5

	// defaultMinCertGracePeriod is the default minimum grace period for workload cert rotation.
	defaultMinCertGracePeriod = 10 * time.Minute

	// Default CA certificate path
	// Currently, custom CA path is not supported; no API to get custom CA cert yet.
	defaultCACertPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

var (
	// dnsCertDir is the location to save generated DNS certificates.
	// TODO: we can probably avoid saving, but will require deeper changes.
	dnsCertDir  = "./var/run/secrets/istio-dns"
	dnsKeyFile  = path.Join(dnsCertDir, "key.pem")
	dnsCertFile = path.Join(dnsCertDir, "cert-chain.pem")
)

// CertController can create certificates signed by K8S server.
func (s *Server) initCertController(args *PilotArgs) error {
	var err error
	var secretNames, dnsNames, namespaces []string

	meshConfig := s.environment.Mesh()
	if meshConfig.GetCertificates() == nil || len(meshConfig.GetCertificates()) == 0 {
		log.Info("nil certificate config")
		return nil
	}

	k8sClient := s.kubeClient
	for _, c := range meshConfig.GetCertificates() {
		name := strings.Join(c.GetDnsNames(), ",")
		if len(name) == 0 { // must have a DNS name
			continue
		}
		if len(c.GetSecretName()) > 0 {
			// Chiron will generate the key and certificate and save them in a secret
			secretNames = append(secretNames, c.GetSecretName())
			dnsNames = append(dnsNames, name)
			namespaces = append(namespaces, args.Namespace)
		}
	}

	// Provision and manage the certificates for non-Pilot services.
	// If services are empty, the certificate controller will do nothing.
	s.certController, err = chiron.NewWebhookController(defaultCertGracePeriodRatio, defaultMinCertGracePeriod,
		k8sClient.CoreV1(), k8sClient.AdmissionregistrationV1beta1(), k8sClient.CertificatesV1beta1(),
		defaultCACertPath, secretNames, dnsNames, namespaces)
	if err != nil {
		return fmt.Errorf("failed to create certificate controller: %v", err)
	}
	s.addStartFunc(func(stop <-chan struct{}) error {
		go func() {
			// Run Chiron to manage the lifecycles of certificates
			s.certController.Run(stop)
		}()

		return nil
	})

	return nil
}

// initDNSCerts will create the certificates to be used by Istiod GRPC server and webhooks, signed by K8S server.
// If the certificate creation fails - for example no support in K8S - returns an error.
// Will use the mesh.yaml DiscoveryAddress to find the default expected address of the control plane,
// with an environment variable allowing override.
//
// Controlled by features.IstiodService env variable, which defines the name of the service to use in the DNS
// cert, or empty for disabling this feature.
//
// TODO: If the discovery address in mesh.yaml is set to port 15012 (XDS-with-DNS-certs) and the name
// matches the k8s namespace, failure to start DNS server is a fatal error.
func (s *Server) initDNSCerts(hostname string) error {
	if _, err := os.Stat(dnsKeyFile); err == nil {
		// Existing certificate mounted by user. Skip self-signed certificate generation.
		// Use this with an existing CA - the expectation is that the cert will match the
		// DNS name in DiscoveryAddress.
		return nil
	}

	parts := strings.Split(hostname, ".")
	if len(parts) < 2 {
		return fmt.Errorf("invalid hostname %s, should contain at least service name and namespace", hostname)
	}
	// Names in the Istiod cert - support the old service names as well.
	// The first is the recommended one, also used by Apiserver for webhooks.
	names := []string{hostname}

	// Default value, matching old installs. For migration we also add the new SAN, so workloads
	// can switch between the names.
	if hostname == "istio-pilot.istio-system.svc" {
		names = append(names, "istiod.istio-system.svc")
	}
	// New name - while migrating we need to support the old name.
	// Both cases will be removed after 1 release, when the move to the new name is completed.
	if hostname == "istiod.istio-system.svc" {
		names = append(names, "istio-pilot.istio-system.svc")
	}

	log.Infoa("Generating K8S-signed cert for ", names)

	// TODO: fallback to citadel (or custom CA) if K8S signing is broken
	certChain, keyPEM, _, err := chiron.GenKeyCertK8sCA(s.kubeClient.CertificatesV1beta1().CertificateSigningRequests(),
		strings.Join(names, ","), parts[0]+".csr.secret", parts[1], defaultCACertPath)
	if err != nil {
		return err
	}

	// Save the certificates to ./var/run/secrets/istio-dns - this is needed since most of the code we currently
	// use to start grpc and webhooks is based on files. This is a memory-mounted dir.
	if err := os.MkdirAll(dnsCertDir, 0600); err != nil {
		return err
	}
	err = ioutil.WriteFile(dnsKeyFile, keyPEM, 0600)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(dnsCertFile, certChain, 0600)
	if err != nil {
		return err
	}
	log.Infoa("Certificates created in ", dnsCertDir)
	return nil
}
