package client

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	routev1 "github.com/openshift/api/route/v1"

	"github.com/skupperproject/skupper/api/types"
	"github.com/skupperproject/skupper/pkg/kube"
	"github.com/skupperproject/skupper/pkg/qdr"
	"github.com/skupperproject/skupper/pkg/utils"
)

func (cli *VanClient) RouterUpdateVersion(ctx context.Context, hup bool) (bool, error) {
	return cli.RouterUpdateVersionInNamespace(ctx, hup, cli.Namespace)
}

func (cli *VanClient) updateStarted(from string, namespace string, ownerrefs []metav1.OwnerReference) error {
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            "skupper-update-state",
			OwnerReferences: ownerrefs,
		},
		Data: map[string]string{
			"from": from,
		},
	}
	_, err := cli.KubeClient.CoreV1().ConfigMaps(namespace).Create(cm)
	if err != nil {
		return err
	}
	return nil
}

func (cli *VanClient) updateCompleted(namespace string) error {
	return cli.KubeClient.CoreV1().ConfigMaps(namespace).Delete("skupper-update-state", &metav1.DeleteOptions{})
}

func (cli *VanClient) isUpdating(namespace string) (bool, string, error) {
	cm, err := cli.KubeClient.CoreV1().ConfigMaps(namespace).Get("skupper-update-state", metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return false, "", nil
	} else if err != nil {
		return false, "", err
	}
	return true, cm.Data["from"], nil
}

func (cli *VanClient) RouterUpdateVersionInNamespace(ctx context.Context, hup bool, namespace string) (bool, error) {
	configmap, err := cli.KubeClient.CoreV1().ConfigMaps(namespace).Get(types.TransportConfigMapName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	config, err := qdr.GetRouterConfigFromConfigMap(configmap)
	if err != nil {
		return false, err
	}
	site := config.GetSiteMetadata()
	//compare to version of library running
	updateSite := false
	if utils.LessRecentThanVersion(Version, site.Version) {
		// site is newer than client library, cannot update
		return false, fmt.Errorf("Site (%s) is newer than library (%s); cannot update", site.Version, Version)
	}
	rename := false
	inprogress, originalVersion, err := cli.isUpdating(namespace)
	if err != nil {
		return false, err
	}
	if inprogress {
		rename = utils.LessRecentThanVersion(originalVersion, "0.5.0")
	}
	if utils.MoreRecentThanVersion(Version, site.Version) || (utils.EquivalentVersion(Version, site.Version) && Version != site.Version) {
		if !inprogress && utils.LessRecentThanVersion(site.Version, "0.5.0") {
			rename = true
			err = cli.updateStarted(site.Version, namespace, configmap.ObjectMeta.OwnerReferences)
			if err != nil {
				return false, err
			}
			inprogress = true
		}

		// site is marked as older than library, need to update
		updateSite = true

		site.Version = Version
		config.SetSiteMetadata(&site)

		_, err = config.UpdateConfigMap(configmap)
		if err != nil {
			return false, err
		}
		_, err = cli.KubeClient.CoreV1().ConfigMaps(namespace).Update(configmap)
		if err != nil {
			return false, err
		}
	}
	usingRoutes := false
	consoleUsesLoadbalancer := false
	routerExposedAsIp := false
	if rename {
		//create new resources (as copies of old ones)
		// services
		_, err = kube.CopyService("skupper-messaging", types.LocalTransportServiceName, map[string]string{}, namespace, cli.KubeClient)
		if err != nil && !errors.IsAlreadyExists(err) {
			return false, err
		}
		_, err = kube.CopyService("skupper-internal", types.TransportServiceName, map[string]string{}, namespace, cli.KubeClient)
		if err != nil && !errors.IsAlreadyExists(err) {
			return false, err
		}
		servingCertsAnnotation := map[string]string{
			"service.alpha.openshift.io/serving-cert-secret-name": types.OauthConsoleSecret,
		}
		controllerSvc, err := kube.CopyService("skupper-controller", types.ControllerServiceName, servingCertsAnnotation, namespace, cli.KubeClient)
		if err != nil && !errors.IsAlreadyExists(err) {
			return false, err
		}
		if controllerSvc != nil {
			consoleUsesLoadbalancer = controllerSvc.Spec.Type == corev1.ServiceTypeLoadBalancer
		}
		//update annotation on skupper-router-console if it exists
		routerConsoleService, err := cli.KubeClient.CoreV1().Services(namespace).Get(types.RouterConsoleServiceName, metav1.GetOptions{})
		if err == nil {
			if routerConsoleService.ObjectMeta.Annotations == nil {
				routerConsoleService.ObjectMeta.Annotations = map[string]string{}
			}
			routerConsoleService.ObjectMeta.Annotations["service.alpha.openshift.io/serving-cert-secret-name"] = types.OauthRouterConsoleSecret
			_, err := cli.KubeClient.CoreV1().Services(namespace).Update(routerConsoleService)
			if err != nil {
				return false, err
			}
		}

		// secrets
		// ca's just need to be copied to new secret
		err = kube.CopySecret("skupper-ca", types.LocalCaSecret, namespace, cli.KubeClient)
		if err != nil && !errors.IsAlreadyExists(err) {
			return false, err
		}
		err = kube.CopySecret("skupper-internal-ca", types.SiteCaSecret, namespace, cli.KubeClient)
		if err != nil && !errors.IsAlreadyExists(err) {
			return false, err
		}
		// credentials need to be regenerated to be valid for new service names
		credentials := []types.Credential{}
		credentials = append(credentials, types.Credential{
			CA:          types.LocalCaSecret,
			Name:        types.LocalServerSecret,
			Subject:     types.LocalTransportServiceName,
			Hosts:       []string{types.LocalTransportServiceName, qualifiedServiceName(types.LocalTransportServiceName, namespace)},
			ConnectJson: false,
		})
		credentials = append(credentials, types.Credential{
			CA:          types.LocalCaSecret,
			Name:        types.LocalClientSecret,
			Subject:     types.LocalTransportServiceName,
			Hosts:       []string{},
			ConnectJson: true,
		})

		usingRoutes, err = cli.usingRoutes(namespace)
		if usingRoutes {
			//no need to regenerate certificate as route names have not changed
			err = kube.CopySecret("skupper-internal", types.SiteServerSecret, namespace, cli.KubeClient)
			if err != nil && !errors.IsAlreadyExists(err) {
				return false, err
			}
		} else {
			hosts, err := cli.getTransportHosts(namespace)
			if err != nil {
				return false, err
			}
			if len(hosts) > 0 {
				ip := net.ParseIP(hosts[0])
				if ip != nil {
					routerExposedAsIp = true
				}
			}

			subject := types.TransportServiceName
			for _, host := range hosts {
				if len(host) < 64 {
					subject = host
					break
				}
			}
			credentials = append(credentials, types.Credential{
				CA:          types.SiteCaSecret,
				Name:        types.SiteServerSecret,
				Subject:     subject,
				Hosts:       hosts,
				ConnectJson: false,
			})
		}
		for _, cred := range credentials {
			var owner *metav1.OwnerReference
			if len(configmap.ObjectMeta.OwnerReferences) > 0 {
				owner = &configmap.ObjectMeta.OwnerReferences[0]
			}
			kube.NewSecret(cred, owner, namespace, cli.KubeClient)
		}

		// serviceaccounts
		err = kube.CopyServiceAccount("skupper", types.TransportServiceAccountName, map[string]string{}, namespace, cli.KubeClient)
		if err != nil && !errors.IsAlreadyExists(err) {
			return false, err
		}
		annotationSubstitutions := map[string]string{
			"serviceaccounts.openshift.io/oauth-redirectreference.primary": "{\"kind\":\"OAuthRedirectReference\",\"apiVersion\":\"v1\",\"reference\":{\"kind\":\"Route\",\"name\":\"" + types.ConsoleRouteName + "\"}}",
		}
		err = kube.CopyServiceAccount("skupper-proxy-controller", types.ControllerServiceAccountName, annotationSubstitutions, namespace, cli.KubeClient)
		if err != nil && !errors.IsAlreadyExists(err) {
			return false, err
		}

		// roles
		controllerRole := &rbacv1.Role{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "rbac.authorization.k8s.io/v1",
				Kind:       "Role",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:            types.ControllerRoleName,
				OwnerReferences: configmap.ObjectMeta.OwnerReferences,
			},
			Rules: types.ControllerPolicyRule,
		}
		_, err = kube.CreateRole(namespace, controllerRole, cli.KubeClient)
		if err != nil && !errors.IsAlreadyExists(err) {
			return false, err
		}

		err = kube.CopyRole("skupper-view", types.TransportRoleName, namespace, cli.KubeClient)
		if err != nil && !errors.IsAlreadyExists(err) {
			return false, err
		}

		// rolebindings
		rolebindings := []rbacv1.RoleBinding{
			{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "rbac.authorization.k8s.io/v1",
					Kind:       "RoleBinding",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:            types.ControllerRoleBindingName,
					OwnerReferences: configmap.ObjectMeta.OwnerReferences,
				},
				Subjects: []rbacv1.Subject{{
					Kind: "ServiceAccount",
					Name: types.ControllerServiceAccountName,
				}},
				RoleRef: rbacv1.RoleRef{
					Kind: "Role",
					Name: types.ControllerRoleName,
				},
			},
			{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "rbac.authorization.k8s.io/v1",
					Kind:       "RoleBinding",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:            types.TransportRoleBindingName,
					OwnerReferences: configmap.ObjectMeta.OwnerReferences,
				},
				Subjects: []rbacv1.Subject{{
					Kind: "ServiceAccount",
					Name: types.TransportServiceAccountName,
				}},
				RoleRef: rbacv1.RoleRef{
					Kind: "Role",
					Name: types.TransportRoleName,
				},
			},
		}
		for _, rolebinding := range rolebindings {
			_, err = kube.CreateRoleBinding(namespace, &rolebinding, cli.KubeClient)
			if err != nil && !errors.IsAlreadyExists(err) {
				return false, err
			}
		}

		if cli.RouteClient != nil {
			//routes: skupper-controller -> skupper
			original, err := cli.RouteClient.Routes(namespace).Get("skupper-controller", metav1.GetOptions{})
			if err == nil {
				route := &routev1.Route{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Route",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:            types.ConsoleRouteName,
						OwnerReferences: original.ObjectMeta.OwnerReferences,
					},
					Spec: routev1.RouteSpec{
						Path: original.Spec.Path,
						Port: original.Spec.Port,
						TLS:  original.Spec.TLS,
						To: routev1.RouteTargetReference{
							Kind: "Service",
							Name: types.ControllerServiceName,
						},
					},
				}
				_, err := cli.RouteClient.Routes(namespace).Create(route)
				if err != nil && !errors.IsAlreadyExists(err) {
					return false, err
				}
			} else if !errors.IsNotFound(err) {
				return false, err
			}
			//need to update edge and inter-router routes to point at different service:
			err = kube.UpdateTargetServiceForRoute(types.EdgeRouteName, types.TransportServiceName, namespace, cli.RouteClient)
			if err != nil {
				return false, err
			}
			err = kube.UpdateTargetServiceForRoute(types.InterRouterRouteName, types.TransportServiceName, namespace, cli.RouteClient)
			if err != nil {
				return false, err
			}
		}
	}

	router, err := cli.KubeClient.AppsV1().Deployments(namespace).Get(types.TransportDeploymentName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	updateRouter := false
	if rename {
		//update deployment
		// - serviceaccount
		router.Spec.Template.Spec.ServiceAccountName = types.TransportServiceAccountName
		// - mounted secrets:
		kube.UpdateSecretVolume(&router.Spec.Template.Spec, "skupper-amqps", types.LocalServerSecret)
		kube.UpdateSecretVolume(&router.Spec.Template.Spec, "skupper-internal", types.SiteServerSecret)
		kube.UpdateSecretVolume(&router.Spec.Template.Spec, "skupper-proxy-certs", types.OauthRouterConsoleSecret)
		// -oauth proxy sidecar
		updateOauthProxyServiceAccount(&router.Spec.Template.Spec, types.TransportServiceAccountName)

		updateRouter = true
	}
	desiredRouterImage := GetRouterImageName()
	if router.Spec.Template.Spec.Containers[0].Image != desiredRouterImage {
		router.Spec.Template.Spec.Containers[0].Image = desiredRouterImage
		updateRouter = true
	}
	if updateRouter || updateSite || hup {
		if !updateRouter {
			//need to trigger a router redployment to pick up the revised metadata field
			touch(router)
			updateRouter = true
		}
		_, err = cli.KubeClient.AppsV1().Deployments(namespace).Update(router)
		if err != nil {
			return false, err
		}
		if routerExposedAsIp {
			fmt.Println("Sites previously linked to this one will require new tokens")
		}
	}

	controller, err := cli.KubeClient.AppsV1().Deployments(namespace).Get(types.ControllerDeploymentName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	updateController := false
	if rename {
		//update deployment
		// - serviceaccount
		controller.Spec.Template.Spec.ServiceAccountName = types.ControllerServiceAccountName
		// - mounted secrets:
		kube.UpdateSecretVolume(&controller.Spec.Template.Spec, "skupper", types.LocalClientSecret)
		kube.UpdateSecretVolume(&controller.Spec.Template.Spec, "skupper-controller-certs", types.OauthConsoleSecret)
		// -oauth proxy sidecar
		updateOauthProxyServiceAccount(&controller.Spec.Template.Spec, types.ControllerServiceAccountName)
		updateController = true
	}
	desiredControllerImage := GetServiceControllerImageName()
	if controller.Spec.Template.Spec.Containers[0].Image != desiredControllerImage {
		controller.Spec.Template.Spec.Containers[0].Image = desiredControllerImage
		updateController = true
	}
	if updateController || hup {
		if !updateController {
			//trigger redeployment of service-controller to pick up latest image
			touch(controller)
			updateController = true
		}
		_, err = cli.KubeClient.AppsV1().Deployments(namespace).Update(controller)
		if err != nil {
			return false, err
		}
		if consoleUsesLoadbalancer {
			host := ""
			for i := 0; host == "" && i < 120; i++ {
				if i > 0 {
					time.Sleep(time.Second)
				}
				service, err := kube.GetService(types.ControllerServiceName, namespace, cli.KubeClient)
				if err != nil {
					fmt.Println("Could not determine new console url:", err.Error())
					break
				}
				host = kube.GetLoadBalancerHostOrIP(service)
			}
			if host != "" {
				fmt.Println("Console is now at", "http://"+host+":8080")
			}
		}
	}
	if rename {
		//delete old resources
		if cli.RouteClient != nil {
			err = cli.RouteClient.Routes(namespace).Delete("skupper-controller", &metav1.DeleteOptions{})
			if err != nil && !errors.IsNotFound(err) {
				return false, err
			}
		}

		services := []string{
			"skupper-messaging",
			"skupper-controller",
		}
		if usingRoutes {
			//only delete skupper-internal if using
			//routes, as otherwise previously issued
			//tokens will reference it
			services = append(services, "skupper-internal")
		}
		for _, service := range services {
			err = cli.KubeClient.CoreV1().Services(namespace).Delete(service, &metav1.DeleteOptions{})
			if err != nil && !errors.IsNotFound(err) {
				return false, err
			}
		}

		secrets := []string{
			"skupper",
			"skupper-amqps",
			"skupper-ca",
			"skupper-internal",
			"skupper-internal-ca",
		}
		for _, secret := range secrets {
			err = cli.KubeClient.CoreV1().Secrets(namespace).Delete(secret, &metav1.DeleteOptions{})
			if err != nil && !errors.IsNotFound(err) {
				return false, err
			}
		}

		rolebindings := []string{
			"skupper-proxy-controller-skupper-edit",
			"skupper-skupper-view",
		}
		for _, rolebinding := range rolebindings {
			err = cli.KubeClient.RbacV1().RoleBindings(namespace).Delete(rolebinding, &metav1.DeleteOptions{})
			if err != nil && !errors.IsNotFound(err) {
				return false, err
			}
		}
		serviceAccounts := []string{
			"skupper",
			"skupper-proxy-controller",
		}
		for _, serviceAccount := range serviceAccounts {
			err = cli.KubeClient.CoreV1().ServiceAccounts(namespace).Delete(serviceAccount, &metav1.DeleteOptions{})
			if err != nil && !errors.IsNotFound(err) {
				return false, err
			}
		}
		roles := []string{
			"skupper-edit",
			"skupper-view",
		}
		for _, role := range roles {
			err = cli.KubeClient.RbacV1().Roles(namespace).Delete(role, &metav1.DeleteOptions{})
			if err != nil && !errors.IsNotFound(err) {
				return false, err
			}
		}
	}
	if inprogress {
		err = cli.updateCompleted(namespace)
		if err != nil {
			return true, err
		}
	}
	return updateRouter || updateController || updateSite, nil
}

func (cli *VanClient) RouterUpdateLogging(ctx context.Context, settings *corev1.ConfigMap, hup bool) (bool, error) {
	siteConfig, err := cli.SiteConfigInspect(ctx, settings)
	if err != nil {
		return false, err
	}
	configmap, err := cli.KubeClient.CoreV1().ConfigMaps(settings.ObjectMeta.Namespace).Get(types.TransportConfigMapName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	routerConfig, err := qdr.GetRouterConfigFromConfigMap(configmap)
	if err != nil {
		return false, err
	}
	updated := configureRouterLogging(routerConfig, siteConfig.Spec.RouterLogging)
	if updated {
		routerConfig.WriteToConfigMap(configmap)
		_, err = cli.KubeClient.CoreV1().ConfigMaps(settings.ObjectMeta.Namespace).Update(configmap)
		if err != nil {
			return false, err
		}
		if hup {
			router, err := cli.KubeClient.AppsV1().Deployments(settings.ObjectMeta.Namespace).Get(types.TransportDeploymentName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			touch(router)
			_, err = cli.KubeClient.AppsV1().Deployments(settings.ObjectMeta.Namespace).Update(router)
			if err != nil {
				return false, err
			}
		}
		return true, nil
	}
	return false, nil
}

func (cli *VanClient) RouterUpdateDebugMode(ctx context.Context, settings *corev1.ConfigMap) (bool, error) {
	siteConfig, err := cli.SiteConfigInspect(ctx, settings)
	if err != nil {
		return false, err
	}
	router, err := cli.KubeClient.AppsV1().Deployments(settings.ObjectMeta.Namespace).Get(types.TransportDeploymentName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	current := kube.GetEnvVarForDeployment(router, "QDROUTERD_DEBUG")
	if current == siteConfig.Spec.RouterDebugMode {
		return false, nil
	}
	if siteConfig.Spec.RouterDebugMode == "" {
		kube.DeleteEnvVarForDeployment(router, "QDROUTERD_DEBUG")
	} else {
		kube.SetEnvVarForDeployment(router, "QDROUTERD_DEBUG", siteConfig.Spec.RouterDebugMode)
	}
	_, err = cli.KubeClient.AppsV1().Deployments(settings.ObjectMeta.Namespace).Update(router)
	if err != nil {
		return false, err
	}
	return true, nil

}

func (cli *VanClient) updateAnnotationsOnDeployment(ctx context.Context, namespace string, name string, annotations map[string]string) (bool, error) {
	deployment, err := cli.KubeClient.AppsV1().Deployments(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(annotations, deployment.Spec.Template.ObjectMeta.Annotations) {
		deployment.Spec.Template.ObjectMeta.Annotations = annotations
		_, err = cli.KubeClient.AppsV1().Deployments(namespace).Update(deployment)
		if err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (cli *VanClient) RouterUpdateAnnotations(ctx context.Context, settings *corev1.ConfigMap) (bool, error) {
	siteConfig, err := cli.SiteConfigInspect(ctx, settings)
	if err != nil {
		return false, err
	}
	updated, err := cli.updateAnnotationsOnDeployment(ctx, settings.ObjectMeta.Namespace, types.ControllerDeploymentName, siteConfig.Spec.Annotations)
	if err != nil {
		return updated, err
	}
	transportAnnotations := map[string]string{}
	for key, value := range types.TransportPrometheusAnnotations {
		transportAnnotations[key] = value
	}
	for key, value := range siteConfig.Spec.Annotations {
		transportAnnotations[key] = value
	}
	updated, err = cli.updateAnnotationsOnDeployment(ctx, settings.ObjectMeta.Namespace, types.TransportDeploymentName, transportAnnotations)
	if err != nil {
		return updated, err
	}
	return updated, nil
}

func (cli *VanClient) RouterRestart(ctx context.Context, namespace string) error {
	router, err := cli.KubeClient.AppsV1().Deployments(namespace).Get(types.TransportDeploymentName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	touch(router)
	_, err = cli.KubeClient.AppsV1().Deployments(namespace).Update(router)
	return err
}

func touch(deployment *appsv1.Deployment) {
	if deployment.Spec.Template.ObjectMeta.Annotations == nil {
		deployment.Spec.Template.ObjectMeta.Annotations = map[string]string{}
	}
	deployment.Spec.Template.ObjectMeta.Annotations[types.UpdatedAnnotation] = time.Now().Format(time.RFC1123Z)

}

func updateOauthProxyServiceAccount(spec *corev1.PodSpec, name string) {
	if len(spec.Containers) > 1 && spec.Containers[1].Name == "oauth-proxy" {
		for i, arg := range spec.Containers[1].Args {
			if strings.HasPrefix(arg, "--openshift-service-account") {
				spec.Containers[1].Args[i] = "--openshift-service-account=" + name
			}
		}
	}
}

func (cli *VanClient) usingRoutes(namespace string) (bool, error) {
	if cli.RouteClient != nil {
		_, err := kube.GetRoute(types.InterRouterRouteName, namespace, cli.RouteClient)
		if err == nil {
			return true, nil
		} else if errors.IsNotFound(err) {
			return false, nil
		} else {
			return false, err
		}
	} else {
		return false, nil
	}
}

func (cli *VanClient) getTransportHosts(namespace string) ([]string, error) {
	hosts := []string{}
	oldService, err := kube.GetService("skupper-internal", namespace, cli.KubeClient)
	if err != nil {
		return nil, err
	}
	if oldService.Spec.Type == corev1.ServiceTypeLoadBalancer {
		host := ""
		for i := 0; i < 120; i++ {
			if i > 0 {
				time.Sleep(time.Second)
			}
			service, err := kube.GetService(types.TransportServiceName, namespace, cli.KubeClient)
			if err != nil {
				return nil, err
			}
			host = kube.GetLoadBalancerHostOrIP(service)
			if host != "" {
				hosts = append(hosts, host)
				break
			}
		}
		host = kube.GetLoadBalancerHostOrIP(oldService)
		if host != "" {
			hosts = append(hosts, host)
		}
	}
	hosts = append(hosts, types.TransportServiceName)
	hosts = append(hosts, qualifiedServiceName(types.TransportServiceName, namespace))
	hosts = append(hosts, qualifiedServiceName("skupper-internal", namespace))
	return hosts, nil
}

func qualifiedServiceName(name string, namespace string) string {
	return name + "." + namespace + ".svc.cluster.local"
}
