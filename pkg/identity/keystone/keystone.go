/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package keystone

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/golang/glog"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	netutil "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	certutil "k8s.io/client-go/util/cert"
)

// Construct a Keystone v3 client, bail out if we cannot find the v3 API endpoint
func createIdentityV3Provider(options gophercloud.AuthOptions, transport http.RoundTripper) (*gophercloud.ProviderClient, error) {
	client, err := openstack.NewClient(options.IdentityEndpoint)
	if err != nil {
		return nil, err
	}

	if transport != nil {
		client.HTTPClient.Transport = transport
	}

	versions := []*utils.Version{
		{ID: "v3", Priority: 30, Suffix: "/v3/"},
	}
	chosen, _, err := utils.ChooseVersion(client, versions)
	if err != nil {
		return nil, fmt.Errorf("Unable to find identity API v3 version : %v", err)
	}

	switch chosen.ID {
	case "v3":
		return client, nil
	default:
		// The switch statement must be out of date from the versions list.
		return nil, fmt.Errorf("Unsupported identity API version: %s", chosen.ID)
	}
}

func createKubernetesClient(kubeConfig string) (*kubernetes.Clientset, error) {
	glog.Info("Creating kubernetes API client.")

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		return nil, err
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	v, err := client.Discovery().ServerVersion()
	if err != nil {
		return nil, err
	}

	glog.Infof("Kubernetes API client created, server version %s", fmt.Sprintf("v%v.%v", v.Major, v.Minor))
	return client, nil
}

func createKeystoneClient(authURL string, caFile string) (*gophercloud.ServiceClient, error) {
	// FIXME: Enable this check later
	//if !strings.HasPrefix(authURL, "https") {
	//	return nil, errors.New("Auth URL should be secure and start with https")
	//}
	var transport http.RoundTripper
	if authURL == "" {
		return nil, errors.New("Auth URL is empty")
	}
	if caFile != "" {
		roots, err := certutil.NewPool(caFile)
		if err != nil {
			return nil, err
		}
		config := &tls.Config{}
		config.RootCAs = roots
		transport = netutil.SetOldTransportDefaults(&http.Transport{TLSClientConfig: config})
	}
	opts := gophercloud.AuthOptions{IdentityEndpoint: authURL}
	provider, err := createIdentityV3Provider(opts, transport)
	if err != nil {
		return nil, err
	}

	// We should use the V3 API
	client, err := openstack.NewIdentityV3(provider, gophercloud.EndpointOpts{})
	if err != nil {
		glog.Warningf("Failed: Unable to use keystone v3 identity service: %v", err)
		return nil, errors.New("Failed to authenticate")
	}

	// Make sure we look under /v3 for resources
	client.IdentityBase = client.IdentityEndpoint
	client.Endpoint = client.IdentityEndpoint
	return client, nil
}

// NewKeystoneAuthenticator returns a password authenticator that validates credentials using openstack keystone
func NewKeystoneAuthenticator(authURL string, caFile string) (*Authenticator, error) {
	client, err := createKeystoneClient(authURL, caFile)
	if err != nil {
		return nil, err
	}

	return &Authenticator{authURL: authURL, client: client}, nil
}

// NewKeystoneAuthorizer returns a password authorizer that checks whether the user can perform an operation
func NewKeystoneAuthorizer(authURL string, caFile string, policyFile string, configMap string, kubeConfig string) (*Authorizer, error) {
	client, err := createKeystoneClient(authURL, caFile)
	if err != nil {
		return nil, err
	}

	var policy policyList

	if policyFile != "" {
		policy, err = newFromFile(policyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to extract policy from policy file %s: %v", policyFile, err)
		}
	} else if configMap != "" {
		k8sClient, err := createKubernetesClient(kubeConfig)
		if err != nil {
			return nil, err
		}

		cm, err := k8sClient.CoreV1().ConfigMaps("kube-system").Get(configMap, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		if err := json.Unmarshal([]byte(cm.Data["policies"]), &policy); err != nil {
			return nil, fmt.Errorf("failed to parse policies defined in the configmap %s: %v", configMap, err)
		}
	} else {
		return nil, nil
	}

	output, err := json.MarshalIndent(policy, "", "  ")
	if err == nil {
		glog.V(6).Infof("Policy %s", string(output))
	} else {
		glog.V(6).Infof("Error %#v", err)
	}

	return &Authorizer{authURL: authURL, client: client, pl: policy}, nil
}
