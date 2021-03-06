/*
Copyright (c) 2017, UPMC Enterprises
All rights reserved.
Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:
    * Redistributions of source code must retain the above copyright
      notice, this list of conditions and the following disclaimer.
    * Redistributions in binary form must reproduce the above copyright
      notice, this list of conditions and the following disclaimer in the
      documentation and/or other materials provided with the distribution.
    * Neither the name UPMC Enterprises nor the
      names of its contributors may be used to endorse or promote products
      derived from this software without specific prior written permission.
THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL UPMC ENTERPRISES BE LIABLE FOR ANY
DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
(INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES;
LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND
ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
*/

package k8sutil

import (
	"os"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/upmc-enterprises/kong-operator/pkg/tpr"

	k8serrors "k8s.io/client-go/pkg/api/errors"
	"k8s.io/client-go/pkg/fields"
	"k8s.io/client-go/pkg/util/intstr"

	"k8s.io/client-go/kubernetes"
	coreType "k8s.io/client-go/kubernetes/typed/core/v1"
	extensionsType "k8s.io/client-go/kubernetes/typed/extensions/v1beta1"
	"k8s.io/client-go/pkg/api"
	"k8s.io/client-go/pkg/api/unversioned"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/pkg/runtime"
	"k8s.io/client-go/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	tprName = "kong-cluster.enterprises.upmc.com"
)

const (
	kongProxyServiceName   = "kong-proxy"
	kongAdminServiceName   = "kong-admin"
	kongDeploymentName     = "kong"
	kongPostgresSecretName = "kong-postgres"
)

// KubeInterface abstracts the kubernetes client
type KubeInterface interface {
	Services(namespace string) coreType.ServiceInterface
	ThirdPartyResources() extensionsType.ThirdPartyResourceInterface
	Deployments(namespace string) extensionsType.DeploymentInterface
	ReplicaSets(namespace string) extensionsType.ReplicaSetInterface
	Secrets(namespace string) coreType.SecretInterface
}

// K8sutil defines the kube object
type K8sutil struct {
	Config     *rest.Config
	TprClient  *rest.RESTClient
	Kclient    KubeInterface
	MasterHost string
}

// New creates a new instance of k8sutil
func New(kubeCfgFile, masterHost string) (*K8sutil, error) {

	client, tprclient, err := newKubeClient(kubeCfgFile)

	if err != nil {
		logrus.Fatalf("Could not init Kubernetes client! [%s]", err)
	}

	k := &K8sutil{
		Kclient:    client,
		TprClient:  tprclient,
		MasterHost: masterHost,
	}

	return k, nil
}

func buildConfig(kubeCfgFile string) (*rest.Config, error) {
	if kubeCfgFile != "" {
		logrus.Infof("Using OutOfCluster k8s config with kubeConfigFile: %s", kubeCfgFile)
		return clientcmd.BuildConfigFromFlags("", kubeCfgFile)
	}

	logrus.Info("Using InCluster k8s config")
	return rest.InClusterConfig()
}

func configureTPRClient(config *rest.Config) {
	groupversion := unversioned.GroupVersion{
		Group:   "enterprises.upmc.com",
		Version: "v1",
	}

	config.GroupVersion = &groupversion
	config.APIPath = "/apis"
	config.ContentType = runtime.ContentTypeJSON
	config.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: api.Codecs}

	schemeBuilder := runtime.NewSchemeBuilder(
		func(scheme *runtime.Scheme) error {
			scheme.AddKnownTypes(
				unversioned.GroupVersion{Group: "enterprises.upmc.com", Version: "v1"},
				&tpr.KongCluster{},
				&tpr.KongClusterList{},
				&api.ListOptions{},
				&api.DeleteOptions{},
			)
			return nil
		})

	schemeBuilder.AddToScheme(api.Scheme)
}

func newKubeClient(kubeCfgFile string) (KubeInterface, *rest.RESTClient, error) {

	// Create the client config. Use kubeconfig if given, otherwise assume in-cluster.
	Config, err := buildConfig(kubeCfgFile)
	if err != nil {
		panic(err)
	}

	client, err := kubernetes.NewForConfig(Config)
	if err != nil {
		panic(err)
	}

	// make a new config for our extension's API group, using the first config as a baseline
	var tprconfig *rest.Config
	tprconfig = Config

	configureTPRClient(tprconfig)

	tprclient, err := rest.RESTClientFor(tprconfig)
	if err != nil {
		logrus.Error(err.Error())
		logrus.Error("can not get client to TPR")
		os.Exit(2)
	}

	return client, tprclient, nil
}

// CreateKubernetesThirdPartyResource checks if Kong TPR exists. If not, create
func (k *K8sutil) CreateKubernetesThirdPartyResource() error {

	tpr, err := k.Kclient.ThirdPartyResources().Get(tprName)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			tpr := &v1beta1.ThirdPartyResource{
				ObjectMeta: v1.ObjectMeta{
					Name: tprName,
				},
				Versions: []v1beta1.APIVersion{
					{Name: "v1"},
				},
				Description: "Managed kong clusters",
			}

			_, err := k.Kclient.ThirdPartyResources().Create(tpr)
			if err != nil {
				panic(err)
			}
			logrus.Infof("CREATED TPR: %#v", tpr.ObjectMeta.Name)
		} else {
			panic(err)
		}
	} else {
		logrus.Infof("SKIPPING: already exists %#v", tpr.ObjectMeta.Name)
	}

	return nil
}

// GetKongClusters returns a list of custom clusters defined
func (k *K8sutil) GetKongClusters() ([]tpr.KongCluster, error) {
	kongList := tpr.KongClusterList{}
	var err error

	for {
		err = k.TprClient.Get().Resource("KongClusters").Do().Into(&kongList)

		if err != nil {
			logrus.Error("error getting kong clusters")
			logrus.Error(err)
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}

	return kongList.Items, nil
}

// MonitorKongEvents watches for new or removed clusters
func (k *K8sutil) MonitorKongEvents(stopchan chan struct{}) (<-chan *tpr.KongCluster, <-chan error) {
	events := make(chan *tpr.KongCluster)
	errc := make(chan error, 1)

	source := cache.NewListWatchFromClient(k.TprClient, "kongclusters", api.NamespaceAll, fields.Everything())

	createAddHandler := func(obj interface{}) {
		event := obj.(*tpr.KongCluster)
		event.Type = "ADDED"
		events <- event
	}
	createDeleteHandler := func(obj interface{}) {
		event := obj.(*tpr.KongCluster)
		event.Type = "DELETED"
		events <- event
	}

	updateHandler := func(old interface{}, obj interface{}) {
		event := obj.(*tpr.KongCluster)
		event.Type = "MODIFIED"
		events <- event
	}

	_, controller := cache.NewInformer(
		source,
		&tpr.KongCluster{},
		time.Minute*60,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    createAddHandler,
			UpdateFunc: updateHandler,
			DeleteFunc: createDeleteHandler,
		})

	go controller.Run(stopchan)

	return events, errc
}

// CreateKongProxyService creates the kong proxy service
func (k *K8sutil) CreateKongProxyService(namespace string) error {

	// Check if service exists
	svc, err := k.Kclient.Services(namespace).Get(kongProxyServiceName)

	// Service missing, create
	if len(svc.Name) == 0 {
		logrus.Infof("%s not found, creating...", kongProxyServiceName)

		clientSvc := &v1.Service{
			ObjectMeta: v1.ObjectMeta{
				Name: kongProxyServiceName,
				Labels: map[string]string{
					"name": kongProxyServiceName,
				},
			},
			Spec: v1.ServiceSpec{
				Selector: map[string]string{
					"app": "kong",
				},
				Ports: []v1.ServicePort{
					v1.ServicePort{
						Name:       "kong-proxy",
						Port:       80,
						TargetPort: intstr.FromInt(8000),
						Protocol:   "TCP",
					},
					v1.ServicePort{
						Name:       "kong-proxy-ssl",
						Port:       443,
						TargetPort: intstr.FromInt(8443),
						Protocol:   "TCP",
					},
				},
				Type: v1.ServiceTypeLoadBalancer,
				LoadBalancerSourceRanges: []string{
					"0.0.0.0/0",
				},
			},
		}

		_, err := k.Kclient.Services(namespace).Create(clientSvc)

		if err != nil {
			logrus.Error("Could not create proxy service", err)
			return err
		}
	} else if err != nil {
		logrus.Error("Could not get proxy service! ", err)
		return err
	}

	return nil
}

// CreateKongAdminService creates the kong proxy service
func (k *K8sutil) CreateKongAdminService(namespace string) error {

	// Check if service exists
	svc, err := k.Kclient.Services(namespace).Get(kongAdminServiceName)

	// Service missing, create
	if len(svc.Name) == 0 {
		logrus.Infof("%s not found, creating...", kongAdminServiceName)

		clientSvc := &v1.Service{
			ObjectMeta: v1.ObjectMeta{
				Name: kongAdminServiceName,
				Labels: map[string]string{
					"name": kongAdminServiceName,
				},
			},
			Spec: v1.ServiceSpec{
				Selector: map[string]string{
					"app": "kong",
				},
				Ports: []v1.ServicePort{
					v1.ServicePort{
						Name:       "kong-admin",
						Port:       8444,
						TargetPort: intstr.FromInt(8444),
						Protocol:   "TCP",
					},
				},
				Type: v1.ServiceTypeClusterIP,
			},
		}

		_, err := k.Kclient.Services(namespace).Create(clientSvc)

		if err != nil {
			logrus.Error("Could not create admin service: ", err)
			return err
		}
	} else if err != nil {
		logrus.Error("Could not get admin service: ", err)
		return err
	}

	return nil
}

// DeleteProxyService creates the kong proxy service
func (k *K8sutil) DeleteProxyService(namespace string) error {
	err := k.Kclient.Services(namespace).Delete(kongProxyServiceName, &v1.DeleteOptions{})
	if err != nil {
		logrus.Error("Could not delete service "+kongProxyServiceName+":", err)
	} else {
		logrus.Infof("Delete service: %s", kongProxyServiceName)
	}

	return err
}

// DeleteAdminService creates the kong admin service
func (k *K8sutil) DeleteAdminService(namespace string) error {
	err := k.Kclient.Services(namespace).Delete(kongAdminServiceName, &v1.DeleteOptions{})
	if err != nil {
		logrus.Error("Could not delete service "+kongAdminServiceName+":", err)
	} else {
		logrus.Infof("Delete service: %s", kongAdminServiceName)
	}

	return err
}

// CreateKongDeployment creates the kong deployment
func (k *K8sutil) CreateKongDeployment(baseImage string, replicas *int32, namespace string) error {

	// Check if deployment exists
	deployment, err := k.Kclient.Deployments(namespace).Get(kongDeploymentName)

	if len(deployment.Name) == 0 {
		logrus.Infof("%s not found, creating...", kongDeploymentName)

		deployment := &v1beta1.Deployment{
			ObjectMeta: v1.ObjectMeta{
				Name: kongDeploymentName,
				Labels: map[string]string{
					"name": kongDeploymentName,
				},
			},
			Spec: v1beta1.DeploymentSpec{
				Replicas: replicas,
				Template: v1.PodTemplateSpec{
					ObjectMeta: v1.ObjectMeta{
						Labels: map[string]string{
							"app":  "kong",
							"name": kongDeploymentName,
						},
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							v1.Container{
								Name:  kongDeploymentName,
								Image: baseImage,
								Env: []v1.EnvVar{
									v1.EnvVar{
										Name: "NAMESPACE",
										ValueFrom: &v1.EnvVarSource{
											FieldRef: &v1.ObjectFieldSelector{
												FieldPath: "metadata.namespace",
											},
										},
									},
									v1.EnvVar{
										Name: "KONG_PG_USER",
										ValueFrom: &v1.EnvVarSource{
											SecretKeyRef: &v1.SecretKeySelector{
												Key: "KONG_PG_USER",
												LocalObjectReference: v1.LocalObjectReference{
													Name: kongPostgresSecretName,
												},
											},
										},
									},
									v1.EnvVar{
										Name: "KONG_PG_PASSWORD",
										ValueFrom: &v1.EnvVarSource{
											SecretKeyRef: &v1.SecretKeySelector{
												Key: "KONG_PG_PASSWORD",
												LocalObjectReference: v1.LocalObjectReference{
													Name: kongPostgresSecretName,
												},
											},
										},
									},
									v1.EnvVar{
										Name: "KONG_PG_HOST",
										ValueFrom: &v1.EnvVarSource{
											SecretKeyRef: &v1.SecretKeySelector{
												Key: "KONG_PG_HOST",
												LocalObjectReference: v1.LocalObjectReference{
													Name: kongPostgresSecretName,
												},
											},
										},
									},
									v1.EnvVar{
										Name: "KONG_PG_DATABASE",
										ValueFrom: &v1.EnvVarSource{
											SecretKeyRef: &v1.SecretKeySelector{
												Key: "KONG_PG_DATABASE",
												LocalObjectReference: v1.LocalObjectReference{
													Name: kongPostgresSecretName,
												},
											},
										},
									},
									v1.EnvVar{
										Name: "KONG_HOST_IP",
										ValueFrom: &v1.EnvVarSource{
											FieldRef: &v1.ObjectFieldSelector{
												APIVersion: "v1",
												FieldPath:  "status.podIP",
											},
										},
									},
									v1.EnvVar{
										Name:  "KONG_ADMIN_LISTEN", // Disable non-tls
										Value: "127.0.0.1:8001",
									},
								},
								Command: []string{
									"/bin/sh", "-c",
									"KONG_CLUSTER_ADVERTISE=$(KONG_HOST_IP):7946 KONG_NGINX_DAEMON='off' kong start",
								},
								Ports: []v1.ContainerPort{
									v1.ContainerPort{
										Name:          "proxy",
										ContainerPort: 8000,
										Protocol:      v1.ProtocolTCP,
									},
									v1.ContainerPort{
										Name:          "proxy-ssl",
										ContainerPort: 8443,
										Protocol:      v1.ProtocolTCP,
									},
									v1.ContainerPort{
										Name:          "surf-tcp",
										ContainerPort: 7946,
										Protocol:      v1.ProtocolTCP,
									},
									v1.ContainerPort{
										Name:          "surf-udp",
										ContainerPort: 7946,
										Protocol:      v1.ProtocolUDP,
									},
								},
							},
						},
					},
				},
			},
		}

		_, err := k.Kclient.Deployments(namespace).Create(deployment)

		if err != nil {
			logrus.Error("Could not create kong deployment: ", err)
			return err
		}
	} else {
		if err != nil {
			logrus.Error("Could not get kong deployment! ", err)
			return err
		}

		//scale replicas?
		if deployment.Spec.Replicas != replicas {
			deployment.Spec.Replicas = replicas

			_, err := k.Kclient.Deployments(namespace).Update(deployment)

			if err != nil {
				logrus.Error("Could not scale deployment: ", err)
			}
		}
	}

	return nil
}

// DeleteKongDeployment deletes kong deployment
func (k *K8sutil) DeleteKongDeployment(namespace string) error {

	// Get list of deployments
	deployment, err := k.Kclient.Deployments(namespace).Get(kongDeploymentName)

	if err != nil {
		logrus.Error("Could not get deployments! ", err)
		return err
	}

	//Scale the deployment down to zero (https://github.com/kubernetes/client-go/issues/91)
	deployment.Spec.Replicas = new(int32)
	_, err = k.Kclient.Deployments(namespace).Update(deployment)

	if err != nil {
		logrus.Errorf("Could not scale deployment: %s ", deployment.Name)
	} else {
		logrus.Infof("Scaled deployment: %s to zero", deployment.Name)
	}

	err = k.Kclient.Deployments(namespace).Delete(deployment.Name, &v1.DeleteOptions{})

	if err != nil {
		logrus.Errorf("Could not delete deployments: %s ", deployment.Name)
	} else {
		logrus.Infof("Deleted deployment: %s", deployment.Name)
	}

	// ZZzzzzz...zzzzZZZzzz
	time.Sleep(2 * time.Second)

	// Get list of ReplicaSets
	replicaSets, err := k.Kclient.ReplicaSets(namespace).List(v1.ListOptions{LabelSelector: "app=kong,name=kong"})

	if err != nil {
		logrus.Error("Could not get replica sets! ", err)
	}

	for _, replicaSet := range replicaSets.Items {
		err := k.Kclient.ReplicaSets(namespace).Delete(replicaSet.Name, &v1.DeleteOptions{})

		if err != nil {
			logrus.Errorf("Could not delete replica sets: %s ", replicaSet.Name)
		} else {
			logrus.Infof("Deleted replica set: %s", replicaSet.Name)
		}
	}

	return nil
}
