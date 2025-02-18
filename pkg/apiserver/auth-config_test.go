/*
 * This file is part of the CDI project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2019 Red Hat, Inc.
 *
 */

package apiserver

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"reflect"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	k8scert "k8s.io/client-go/util/cert"
	aggregatorapifake "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/fake"

	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	cdiclientfake "kubevirt.io/containerized-data-importer/pkg/client/clientset/versioned/fake"
	"kubevirt.io/containerized-data-importer/pkg/common"
	"kubevirt.io/containerized-data-importer/pkg/util"
	"kubevirt.io/containerized-data-importer/pkg/util/cert"
	"kubevirt.io/containerized-data-importer/pkg/util/cert/triple"
	cryptowatch "kubevirt.io/containerized-data-importer/pkg/util/tls-crypto-watch"
)

func generateCACert() string {
	keyPair, err := triple.NewCA(util.RandAlphaNum(10))
	Expect(err).ToNot(HaveOccurred())
	return string(cert.EncodeCertPEM(keyPair.Cert))
}

func getAPIServerConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "extension-apiserver-authentication",
			Namespace: "kube-system",
		},
		Data: map[string]string{
			"client-ca-file":                     generateCACert(),
			"requestheader-allowed-names":        "[\"front-proxy-client\"]",
			"requestheader-client-ca-file":       generateCACert(),
			"requestheader-extra-headers-prefix": "[\"X-Remote-Extra-\"]",
			"requestheader-group-headers":        "[\"X-Remote-Group\"]",
			"requestheader-username-headers":     "[\"X-Remote-User\"]",
		},
	}
}

func getAPIServerConfigMapNoAllowedNames() *corev1.ConfigMap {
	cm := getAPIServerConfigMap()
	cm.Data["requestheader-allowed-names"] = "[]"
	return cm
}

func verifyAuthConfig(cm *corev1.ConfigMap, authConfig *AuthConfig) {
	if !reflect.DeepEqual([]byte(cm.Data["client-ca-file"]), authConfig.ClientCABytes) {
		Fail("client-ca-file not stored correctly")
	}

	if !reflect.DeepEqual([]byte(cm.Data["requestheader-client-ca-file"]), authConfig.RequestheaderClientCABytes) {
		Fail("client-ca-file not stored correctly")
	}

	if !reflect.DeepEqual(deserializeStringSlice(cm.Data["requestheader-username-headers"]), authConfig.UserHeaders) {
		Fail("requestheader-username-headers not stored correctly")
	}

	if !reflect.DeepEqual(deserializeStringSlice(cm.Data["requestheader-group-headers"]), authConfig.GroupHeaders) {
		Fail("requestheader-group-headers not stored correctly")
	}

	if !reflect.DeepEqual(deserializeStringSlice(cm.Data["requestheader-extra-headers-prefix"]), authConfig.ExtraPrefixHeaders) {
		Fail("requestheader-extra-headers-prefix not stored correctly")
	}
}

type fakeCertWatcher struct {
	cert *tls.Certificate
}

func (fcw *fakeCertWatcher) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return fcw.cert, nil
}

func NewFakeCertWatcher() CertWatcher {
	certBytes, keyBytes, _ := k8scert.GenerateSelfSignedCertKey("foobar", nil, nil)
	c, _ := tls.X509KeyPair(certBytes, keyBytes)
	return &fakeCertWatcher{cert: &c}
}

var _ = Describe("Auth config tests", func() {
	It("New CDI API server", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		kubeobjects := []runtime.Object{}
		kubeobjects = append(kubeobjects, getAPIServerConfigMap())

		client := k8sfake.NewSimpleClientset(kubeobjects...)
		aggregatorClient := aggregatorapifake.NewSimpleClientset()
		cdiClient := cdiclientfake.NewSimpleClientset()
		authorizer := &testAuthorizer{}
		authConfigWatcher, err := NewAuthConfigWatcher(ctx, client)
		Expect(err).ToNot(HaveOccurred())

		cdiConfigTLSWatcher, err := cryptowatch.NewCdiConfigTLSWatcher(ctx, cdiClient)
		Expect(err).ToNot(HaveOccurred())

		installerLabels := map[string]string{
			common.AppKubernetesPartOfLabel:  "testing",
			common.AppKubernetesVersionLabel: "v0.0.0-tests",
		}
		server, err := NewCdiAPIServer("0.0.0.0", 0, client, aggregatorClient, cdiClient, nil, nil, authorizer, authConfigWatcher, cdiConfigTLSWatcher, nil, installerLabels)
		Expect(err).ToNot(HaveOccurred())

		app := server.(*cdiAPIApp)

		secret, err := client.CoreV1().Secrets("cdi").Get(context.TODO(), "cdi-api-signing-key", metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(secret.Labels[common.AppKubernetesComponentLabel]).To(Equal("storage"))
		Expect(secret.Labels[common.AppKubernetesVersionLabel]).To(Equal(installerLabels[common.AppKubernetesVersionLabel]))

		req, err := http.NewRequest("GET", "/apis", nil)
		Expect(err).ToNot(HaveOccurred())
		rr := httptest.NewRecorder()

		app.container.ServeHTTP(rr, req)

		status := rr.Code
		Expect(status).To(Equal(http.StatusOK))
	})

	It("Auth config update", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cm := getAPIServerConfigMap()
		kubeobjects := []runtime.Object{}
		kubeobjects = append(kubeobjects, cm)

		client := k8sfake.NewSimpleClientset(kubeobjects...)
		aggregatorClient := aggregatorapifake.NewSimpleClientset()
		cdiClient := cdiclientfake.NewSimpleClientset()
		authorizer := &testAuthorizer{}
		acw, err := NewAuthConfigWatcher(ctx, client)
		Expect(err).ToNot(HaveOccurred())

		server, err := NewCdiAPIServer("0.0.0.0", 0, client, aggregatorClient, cdiClient, nil, nil, authorizer, acw, nil, nil, map[string]string{})
		Expect(err).ToNot(HaveOccurred())

		app := server.(*cdiAPIApp)

		verifyAuthConfig(cm, app.authConfigWatcher.GetAuthConfig())

		cm.Data["requestheader-client-ca-file"] = generateCACert()

		cm, err = client.CoreV1().ConfigMaps(metav1.NamespaceSystem).Update(context.TODO(), cm, metav1.UpdateOptions{})
		Expect(err).ToNot(HaveOccurred())

		// behavior of this changed in 16.4 used to wait then check so now explicitly waiting
		time.Sleep(100 * time.Millisecond)
		cache.WaitForCacheSync(ctx.Done(), acw.(*authConfigWatcher).informer.HasSynced)

		verifyAuthConfig(cm, app.authConfigWatcher.GetAuthConfig())
	})

	It("Crypto config update", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		intermediateType := cdiv1.TLSProfileIntermediateType
		oldType := cdiv1.TLSProfileOldType
		cdiConfig := &cdiv1.CDIConfig{
			TypeMeta: metav1.TypeMeta{
				Kind:       "CDIConfig",
				APIVersion: "cdi.kubevirt.io/v1beta1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: common.ConfigName,
			},
			Spec: cdiv1.CDIConfigSpec{
				TLSSecurityProfile: &cdiv1.TLSSecurityProfile{
					Type: oldType,
					Old:  &cdiv1.OldTLSProfile{},
				},
			},
		}

		client := k8sfake.NewSimpleClientset()
		aggregatorClient := aggregatorapifake.NewSimpleClientset()
		cdiClient := cdiclientfake.NewSimpleClientset(cdiConfig)
		authorizer := &testAuthorizer{}
		acw, err := NewAuthConfigWatcher(ctx, client)
		Expect(err).ToNot(HaveOccurred())
		ctw, err := cryptowatch.NewCdiConfigTLSWatcher(ctx, cdiClient)
		Expect(err).ToNot(HaveOccurred())

		_, err = NewCdiAPIServer("0.0.0.0", 0, client, aggregatorClient, cdiClient, nil, nil, authorizer, acw, ctw, nil, map[string]string{})
		Expect(err).ToNot(HaveOccurred())

		// 'Old' has TLS 1.0 as min version
		Expect(int(ctw.GetCdiTLSConfig().MinVersion)).To(Equal(tls.VersionTLS10))
		Expect(ctw.GetCdiTLSConfig().CipherSuites).To(Equal(cryptowatch.CipherSuitesIDs(cdiv1.TLSProfiles[oldType].Ciphers)))

		cdiConfig.Spec.TLSSecurityProfile = nil
		// Should roll us back to 'Intermediate' profile (default) instead of the initial 'Old'
		_, err = cdiClient.CdiV1beta1().CDIConfigs().Update(context.TODO(), cdiConfig, metav1.UpdateOptions{})
		Expect(err).ToNot(HaveOccurred())

		// behavior of this changed in 16.4 used to wait then check so now explicitly waiting
		time.Sleep(100 * time.Millisecond)
		cache.WaitForCacheSync(ctx.Done(), ctw.GetInformer().HasSynced)

		// verify back to TLS 1.2 ('Intermediate' spec)
		Expect(int(ctw.GetCdiTLSConfig().MinVersion)).To(Equal(tls.VersionTLS12))
		Expect(ctw.GetCdiTLSConfig().CipherSuites).To(Equal(cryptowatch.CipherSuitesIDs(cdiv1.TLSProfiles[intermediateType].Ciphers)))
	})

	It("Get TLS config", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cm := getAPIServerConfigMap()
		kubeobjects := []runtime.Object{}
		kubeobjects = append(kubeobjects, cm)

		client := k8sfake.NewSimpleClientset(kubeobjects...)
		aggregatorClient := aggregatorapifake.NewSimpleClientset()
		cdiClient := cdiclientfake.NewSimpleClientset()
		authorizer := &testAuthorizer{}
		acw, err := NewAuthConfigWatcher(ctx, client)
		Expect(err).ToNot(HaveOccurred())

		ctw, err := cryptowatch.NewCdiConfigTLSWatcher(ctx, cdiClient)
		Expect(err).ToNot(HaveOccurred())
		certWatcher := NewFakeCertWatcher()

		server, err := NewCdiAPIServer("0.0.0.0", 0, client, aggregatorClient, cdiClient, nil, nil, authorizer, acw, ctw, certWatcher, map[string]string{})
		Expect(err).ToNot(HaveOccurred())

		app := server.(*cdiAPIApp)

		tlsConfig, err := app.getTLSConfig()
		Expect(err).ToNot(HaveOccurred())

		if !reflect.DeepEqual(tlsConfig.ClientCAs, acw.GetAuthConfig().CertPool) {
			Fail("Client cert pools do not match")
		}

		cdiTLSConfig := ctw.GetCdiTLSConfig()
		Expect(cdiTLSConfig.CipherSuites).ToNot(BeEmpty())
		Expect(tlsConfig.CipherSuites).ToNot(BeEmpty())
		if !reflect.DeepEqual(tlsConfig.CipherSuites, cdiTLSConfig.CipherSuites) {
			Fail("Cipher Suites do not match")
		}
	})

	DescribeTable("Validate client CN", func(f func() *corev1.ConfigMap, name string, allowed bool) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		kubeobjects := []runtime.Object{}
		kubeobjects = append(kubeobjects, f())

		client := k8sfake.NewSimpleClientset(kubeobjects...)
		authConfigWatcher, err := NewAuthConfigWatcher(ctx, client)
		Expect(err).ToNot(HaveOccurred())

		result := authConfigWatcher.GetAuthConfig().ValidateName(name)
		Expect(result).To(Equal(allowed))
	},
		Entry("with allowed names", getAPIServerConfigMap, "front-proxy-client", true),
		Entry("without allowed names", getAPIServerConfigMapNoAllowedNames, "front-proxy-client", true),
		Entry("with allowed names", getAPIServerConfigMap, "foobar", false),
	)
})
