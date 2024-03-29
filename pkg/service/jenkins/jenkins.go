package jenkins

import (
	"context"
	"fmt"
	"github.com/dchest/uniuri"
	gerritApi "github.com/epmd-edp/gerrit-operator/v2/pkg/apis/v2/v1alpha1"
	gerritSpec "github.com/epmd-edp/gerrit-operator/v2/pkg/service/gerrit/spec"
	"github.com/epmd-edp/jenkins-operator/v2/pkg/apis/v2/v1alpha1"
	jenkinsClient "github.com/epmd-edp/jenkins-operator/v2/pkg/client/jenkins"
	jenkinsScriptHelper "github.com/epmd-edp/jenkins-operator/v2/pkg/controller/jenkinsscript/helper"
	"github.com/epmd-edp/jenkins-operator/v2/pkg/helper"
	jenkinsDefaultSpec "github.com/epmd-edp/jenkins-operator/v2/pkg/service/jenkins/spec"
	"github.com/epmd-edp/jenkins-operator/v2/pkg/service/platform"
	platformHelper "github.com/epmd-edp/jenkins-operator/v2/pkg/service/platform/helper"
	keycloakApi "github.com/epmd-edp/keycloak-operator/pkg/apis/v1/v1alpha1"
	keycloakV1Api "github.com/epmd-edp/keycloak-operator/pkg/apis/v1/v1alpha1"
	keycloakControllerHelper "github.com/epmd-edp/keycloak-operator/pkg/controller/helper"
	"github.com/pkg/errors"
	"io/ioutil"
	coreV1Api "k8s.io/api/core/v1"
	authV1Api "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"path/filepath"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

const (
	initContainerName             = "grant-permissions"
	adminCredentialsSecretPostfix = "admin-password"
	adminTokenSecretPostfix       = "admin-token"
	defaultScriptsDirectory       = "scripts"
	defaultSlavesDirectory        = "slaves"
	defaultTemplatesDirectory     = "templates"
	slavesTemplateName            = "jenkins-slaves"
	sharedLibrariesTemplateName   = "config-shared-libraries.tmpl"
	keycloakConfigTemplateName    = "config-keycloak.tmpl"
	defaultScriptConfigMapKey     = "context"
	sshKeyDefaultMountPath        = "/tmp/ssh"
	edpJenkinsRoleName            = "edp-jenkins-role"
	edpJenkinsClusterRoleName     = "edp-jenkins-cluster-role"
)

var log = logf.Log.WithName("jenkins_service")

// JenkinsService interface for Jenkins EDP component
type JenkinsService interface {
	Install(instance v1alpha1.Jenkins) (*v1alpha1.Jenkins, error)
	Configure(instance v1alpha1.Jenkins) (*v1alpha1.Jenkins, bool, error)
	ExposeConfiguration(instance v1alpha1.Jenkins) (*v1alpha1.Jenkins, bool, error)
	Integration(instance v1alpha1.Jenkins) (*v1alpha1.Jenkins, bool, error)
	IsDeploymentReady(instance v1alpha1.Jenkins) (bool, error)
}

// NewJenkinsService function that returns JenkinsService implementation
func NewJenkinsService(platformService platform.PlatformService, k8sClient client.Client, k8sScheme *runtime.Scheme) JenkinsService {
	return JenkinsServiceImpl{platformService: platformService, k8sClient: k8sClient, k8sScheme: k8sScheme}
}

// JenkinsServiceImpl struct fo Jenkins EDP Component
type JenkinsServiceImpl struct {
	platformService platform.PlatformService
	k8sClient       client.Client
	k8sScheme       *runtime.Scheme
}

func (j JenkinsServiceImpl) setAdminSecretInStatus(instance *v1alpha1.Jenkins, value string) (*v1alpha1.Jenkins, error) {
	instance.Status.AdminSecretName = value
	err := j.k8sClient.Status().Update(context.TODO(), instance)
	if err != nil {
		err := j.k8sClient.Update(context.TODO(), instance)
		if err != nil {
			return instance, errors.Wrap(err, "Couldn't set admin secret name in status")
		}
	}
	return instance, nil
}

func (j JenkinsServiceImpl) createKeycloakClient(instance v1alpha1.Jenkins, name string) (*keycloakApi.KeycloakClient, error) {
	keycloakClientObject := &keycloakApi.KeycloakClient{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1.edp.epam.com/v1alpha1",
			Kind:       "KeycloakClient",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
		Spec: keycloakApi.KeycloakClientSpec{
			TargetRealm: instance.Spec.KeycloakSpec.Realm,
			Public:      true,
			ClientId:    instance.Name,
		},
	}

	nsn := types.NamespacedName{
		Namespace: instance.Namespace,
		Name:      name,
	}

	err := j.k8sClient.Get(context.TODO(), nsn, keycloakClientObject)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			err := j.k8sClient.Create(context.TODO(), keycloakClientObject)
			if err != nil {
				return nil, errors.Wrapf(err, "Couldn't create Keycloak client object %v", name)
			}
			log.Info("Keycloak client CR created")
			return keycloakClientObject, nil
		}
		return nil, errors.Wrapf(err, "Couldn't get Keycloak client object %v", name)
	}

	return keycloakClientObject, nil
}

func (j JenkinsServiceImpl) mountGerritCredentials(instance v1alpha1.Jenkins) error {
	options := client.ListOptions{Namespace: instance.Namespace}
	list := &gerritApi.GerritList{}

	err := j.k8sClient.List(context.TODO(), &options, list)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			log.Info(fmt.Sprintf("Gerrit installation is not found in namespace %v", instance.Namespace))
			return nil
		} else {
			return errors.Wrapf(err, fmt.Sprintf("Unable to get Gerrit CRs in namespace %v", instance.Namespace))
		}
	}

	if len(list.Items) == 0 {
		log.Info(fmt.Sprintf("Gerrit installation is not found in namespace %v", instance.Namespace))
		return nil
	}

	gerritCrObject := &list.Items[0]
	gerritSpecName := fmt.Sprintf("%v/%v", gerritSpec.EdpAnnotationsPrefix, gerritSpec.EdpCiUSerSshKeySuffix)
	if val, ok := gerritCrObject.ObjectMeta.Annotations[gerritSpecName]; ok {
		volMount := []coreV1Api.VolumeMount{
			{
				Name:      val,
				MountPath: sshKeyDefaultMountPath,
				ReadOnly:  true,
			},
		}

		mode := int32(400)
		vol := []coreV1Api.Volume{
			{
				Name: val,
				VolumeSource: coreV1Api.VolumeSource{
					Secret: &coreV1Api.SecretVolumeSource{
						SecretName:  val,
						DefaultMode: &mode,
						Items: []coreV1Api.KeyToPath{
							{
								Key:  "id_rsa",
								Path: "id_rsa",
								Mode: &mode,
							},
						},
					},
				},
			},
		}

		err = j.platformService.AddVolumeToInitContainer(instance, initContainerName, vol, volMount)
		if err != nil {
			return errors.Wrapf(err, fmt.Sprintf("Unable to patch Jenkins DC in namespace %v", instance.Namespace))
		}
	}
	return nil
}

func (j JenkinsServiceImpl) createSecret(instance v1alpha1.Jenkins, secretName string, username string, password *string) error {
	var secretPassword string
	if password == nil {
		secretPassword = uniuri.New()
	} else {
		secretPassword = *password
	}
	secretData := map[string][]byte{
		"username": []byte(username),
		"password": []byte(secretPassword),
	}

	err := j.platformService.CreateSecret(instance, secretName, secretData)
	if err != nil {
		return errors.Wrapf(err, "Failed to create Secret %v", secretName)
	}
	return nil
}

//setAnnotation add key:value to current resource annotation
func (j JenkinsServiceImpl) setAnnotation(instance *v1alpha1.Jenkins, key string, value string) {
	if len(instance.Annotations) == 0 {
		instance.ObjectMeta.Annotations = map[string]string{
			key: value,
		}
	} else {
		instance.ObjectMeta.Annotations[key] = value
	}
}

// Integration performs integration Jenkins with other EDP components
func (j JenkinsServiceImpl) Integration(instance v1alpha1.Jenkins) (*v1alpha1.Jenkins, bool, error) {
	if instance.Spec.KeycloakSpec.Enabled {

		host, scheme, err := j.platformService.GetExternalEndpoint(instance.Namespace, instance.Name)
		if err != nil {
			return &instance, false, errors.Wrap(err, "Failed to get route from cluster!")
		}

		webUrl := fmt.Sprintf("%s://%s", scheme, host)
		keycloakClient := keycloakV1Api.KeycloakClient{}
		keycloakClient.Name = instance.Name
		keycloakClient.Namespace = instance.Namespace
		keycloakClient.Spec.ClientId = instance.Name
		keycloakClient.Spec.Public = true
		keycloakClient.Spec.WebUrl = webUrl
		keycloakClient.Spec.RealmRoles = &[]keycloakV1Api.RealmRole{
			{
				Name:      "jenkins-administrators",
				Composite: "administrator",
			},
			{
				Name:      "jenkins-users",
				Composite: "developer",
			},
		}

		err = j.platformService.CreateKeycloakClient(&keycloakClient)
		if err != nil {
			return &instance, false, errors.Wrap(err, "Failed to create Keycloak Client data!")
		}

		keycloakClient, err = j.platformService.GetKeycloakClient(instance.Name, instance.Namespace)
		if err != nil {
			return &instance, false, errors.Wrap(err, "Failed to get Keycloak Client CR!")
		}

		keycloakRealm, err := keycloakControllerHelper.GetOwnerKeycloakRealm(j.k8sClient, keycloakClient.ObjectMeta)
		if err != nil {
			return &instance, false, errors.Wrapf(err, "Failed to get Keycloak Realm for %s client!", keycloakClient.Name)
		}

		if keycloakRealm == nil {
			return &instance, false, errors.New("Keycloak Realm CR in not created yet!")
		}

		keycloak, err := keycloakControllerHelper.GetOwnerKeycloak(j.k8sClient, keycloakRealm.ObjectMeta)
		if err != nil {
			errMsg := fmt.Sprintf("Failed to get owner for %s/%s", keycloakClient.Namespace, keycloakClient.Name)
			return &instance, false, errors.Wrap(err, errMsg)
		}

		if keycloak == nil {
			return &instance, false, errors.New("Keycloak CR is not created yet!")
		}

		directoryPath, err := platformHelper.CreatePathToTemplateDirectory(defaultTemplatesDirectory)
		keycloakCfgFilePath := fmt.Sprintf("%s/%s", directoryPath, keycloakConfigTemplateName)

		jenkinsScriptData := platformHelper.JenkinsScriptData{}
		jenkinsScriptData.RealmName = keycloakRealm.Spec.RealmName
		jenkinsScriptData.KeycloakClientName = keycloakClient.Spec.ClientId
		jenkinsScriptData.KeycloakUrl = keycloak.Spec.Url

		scriptContext, err := platformHelper.ParseTemplate(jenkinsScriptData, keycloakCfgFilePath, keycloakConfigTemplateName)
		if err != nil {
			return &instance, false, err
		}

		configKeycloakName := fmt.Sprintf("%v-%v", instance.Name, "config-keycloak")
		configMapData := map[string]string{defaultScriptConfigMapKey: scriptContext.String()}
		err = j.platformService.CreateConfigMap(instance, configKeycloakName, configMapData)
		if err != nil {
			return &instance, false, err
		}

		_, err = j.platformService.CreateJenkinsScript(instance.Namespace, configKeycloakName)
		if err != nil {
			return &instance, false, err
		}
	}

	err := j.mountGerritCredentials(instance)
	if err != nil {
		return &instance, false, errors.Wrapf(err, "Failed to mount Gerrit credentials")
	}

	return &instance, true, nil
}

// ExposeConfiguration performs exposing Jenkins configuration for other EDP components
func (j JenkinsServiceImpl) ExposeConfiguration(instance v1alpha1.Jenkins) (*v1alpha1.Jenkins, bool, error) {
	upd := false

	jc, err := jenkinsClient.InitJenkinsClient(&instance, j.platformService)
	if err != nil {
		return &instance, upd, errors.Wrap(err, "Failed to init Jenkins REST client")
	}
	if jc == nil {
		return &instance, upd, errors.Wrap(err, "Jenkins returns nil client")
	}

	sl, err := jc.GetSlaves()
	if err != nil {
		return &instance, upd, errors.Wrapf(err, "Unable to get Jenkins slaves list")
	}

	ss := []v1alpha1.Slave{}
	for _, s := range sl {
		ss = append(ss, v1alpha1.Slave{s})
	}

	if !reflect.DeepEqual(instance.Status.Slaves, ss) {
		instance.Status.Slaves = ss
		upd = true
	}

	pr, err := jc.GetJobProvisions()

	ps := []v1alpha1.JobProvision{}
	for _, p := range pr {
		ps = append(ps, v1alpha1.JobProvision{p})
	}

	if !reflect.DeepEqual(instance.Status.JobProvisions, ps) {
		instance.Status.JobProvisions = ps
		upd = true
	}

	return &instance, upd, nil
}

// Configure performs self-configuration of Jenkins
func (j JenkinsServiceImpl) Configure(instance v1alpha1.Jenkins) (*v1alpha1.Jenkins, bool, error) {
	jc, err := jenkinsClient.InitJenkinsClient(&instance, j.platformService)
	if err != nil {
		return &instance, false, errors.Wrap(err, "Failed to init Jenkins REST client")
	}
	if jc == nil {
		return &instance, false, errors.Wrap(err, "Jenkins returns nil client")
	}

	adminTokenSecretName := fmt.Sprintf("%v-%v", instance.Name, adminTokenSecretPostfix)
	adminTokenSecret, err := j.platformService.GetSecretData(instance.Namespace, adminTokenSecretName)
	if err != nil {
		return &instance, false, errors.Wrapf(err, "Unable to get admin token secret for %v", instance.Name)
	}

	if adminTokenSecret == nil {
		token, err := jc.GetAdminToken()
		if err != nil {
			return &instance, false, errors.Wrap(err, "Failed to get token from admin user")
		}

		err = j.createSecret(instance, adminTokenSecretName, jenkinsDefaultSpec.JenkinsDefaultAdminUser, token)
		if err != nil {
			return &instance, false, err
		}

		adminTokenAnnotationKey := helper.GenerateAnnotationKey(jenkinsDefaultSpec.JenkinsTokenAnnotationSuffix)
		j.setAnnotation(&instance, adminTokenAnnotationKey, adminTokenSecretName)

		err = j.k8sClient.Update(context.TODO(), &instance)
		if err != nil {
			return &instance, false, err
		}

		updatedInstance, err := j.setAdminSecretInStatus(&instance, adminTokenSecretName)
		if err != nil {
			return &instance, false, err
		}
		instance = *updatedInstance
	}

	scriptsDirectoryPath, err := platformHelper.CreatePathToTemplateDirectory(defaultScriptsDirectory)
	if err != nil {
		return &instance, false, err
	}

	directory, err := ioutil.ReadDir(scriptsDirectoryPath)
	if err != nil {
		return &instance, false, errors.Wrapf(err, fmt.Sprintf("Couldn't read directory %v", scriptsDirectoryPath))
	}

	for _, file := range directory {
		configMapName := fmt.Sprintf("%v-%v", instance.Name, file.Name())
		configMapKey := jenkinsScriptHelper.JenkinsDefaultScriptConfigMapKey

		path := filepath.FromSlash(fmt.Sprintf("%v/%v", scriptsDirectoryPath, file.Name()))
		jenkinsScript, err := j.platformService.CreateJenkinsScript(instance.Namespace, configMapName)
		if err != nil {
			return &instance, false, err
		}

		err = j.platformService.CreateConfigMapFromFileOrDir(instance, configMapName, &configMapKey, path, jenkinsScript)
		if err != nil {
			errMsg := fmt.Sprintf("Couldn't create configs-map %v in namespace %v.", configMapName, instance.Namespace)
			return &instance, false, errors.Wrap(err, errMsg)
		}
	}

	slavesDirectoryPath, err := platformHelper.CreatePathToTemplateDirectory(defaultSlavesDirectory)
	if err != nil {
		return &instance, false, err
	}

	directory, err = ioutil.ReadDir(slavesDirectoryPath)
	if err != nil {
		return nil, false, errors.Wrapf(err, fmt.Sprintf("Couldn't read directory %v", slavesDirectoryPath))
	}

	JenkinsSlavesConfigmapLabels := map[string]string{
		"role": "jenkins-slave",
	}

	err = j.platformService.CreateConfigMapFromFileOrDir(instance, slavesTemplateName, nil,
		slavesDirectoryPath, &instance, JenkinsSlavesConfigmapLabels)
	if err != nil {
		return nil, false, errors.Wrapf(err, "Couldn't create configs-map %v in namespace %v.",
			slavesTemplateName, instance.Namespace)
	}

	sharedLibrariesDirectoryPath, err := platformHelper.CreatePathToTemplateDirectory(defaultTemplatesDirectory)
	if err != nil {
		return &instance, false, err
	}

	sharedLibrariesFilePath := fmt.Sprintf("%s/%s", sharedLibrariesDirectoryPath, sharedLibrariesTemplateName)

	jenkinsScriptData := platformHelper.JenkinsScriptData{}
	jenkinsScriptData.JenkinsSharedLibraries = instance.Spec.SharedLibraries

	sharedLibrariesContext, err := platformHelper.ParseTemplate(jenkinsScriptData, sharedLibrariesFilePath, sharedLibrariesTemplateName)
	if err != nil {
		return &instance, false, nil
	}

	jenkinsScriptName := "config-shared-libraries"
	configMapName := fmt.Sprintf("%v-%v", instance.Name, jenkinsScriptName)

	_, err = j.platformService.CreateJenkinsScript(instance.Namespace, configMapName)
	if err != nil {
		return &instance, false, err
	}

	configMapData := map[string]string{jenkinsScriptHelper.JenkinsDefaultScriptConfigMapKey: sharedLibrariesContext.String()}
	err = j.platformService.CreateConfigMap(instance, configMapName, configMapData)
	if err != nil {
		return &instance, false, err
	}

	return &instance, true, nil
}

// Install performs installation of Jenkins
func (j JenkinsServiceImpl) Install(instance v1alpha1.Jenkins) (*v1alpha1.Jenkins, error) {
	secretName := fmt.Sprintf("%v-%v", instance.Name, adminCredentialsSecretPostfix)
	err := j.createSecret(instance, secretName, jenkinsDefaultSpec.JenkinsDefaultAdminUser, nil)
	if err != nil {
		return &instance, err
	}
	if instance.Status.AdminSecretName == "" {
		updatedInstance, err := j.setAdminSecretInStatus(&instance, secretName)
		if err != nil {
			return &instance, err
		}
		instance = *updatedInstance
	}

	err = j.platformService.CreateServiceAccount(instance)
	if err != nil {
		return &instance, errors.Wrapf(err, "Failed to create Service Account %v", instance.Name)
	}

	rules := []authV1Api.PolicyRule{
		{
			APIGroups: []string{"*"},
			Resources: []string{"codebases", "codebasebranches", "cdpipelines", "stages", "gitservers", "adminconsoles"},
			Verbs:     []string{"get", "create", "update"},
		},
	}

	err = j.platformService.CreateRole(instance, edpJenkinsRoleName, rules)
	if err != nil {
		return &instance, errors.Wrapf(err, "Failed to create Role %v", edpJenkinsRoleName)
	}

	rules = j.platformService.CreateSCCPolicyRule()

	err = j.platformService.CreateClusterRole(instance, edpJenkinsClusterRoleName, rules)
	if err != nil {
		return &instance, errors.Wrapf(err, "Failed to create ClusterRole %v", edpJenkinsClusterRoleName)
	}

	roleBindingName := fmt.Sprintf("%v-edp-resources-permissions", instance.Name)
	err = j.platformService.CreateUserRoleBinding(instance, roleBindingName, edpJenkinsRoleName, "Role")
	if err != nil {
		return &instance, errors.Wrapf(err, "Failed to create Role Binding %v", instance.Name)
	}

	roleBindingName = fmt.Sprintf("%v-edit-permissions", instance.Name)
	err = j.platformService.CreateUserRoleBinding(instance, roleBindingName, "edit", "ClusterRole")
	if err != nil {
		return &instance, errors.Wrapf(err, "Failed to create Role Binding %v", instance.Name)
	}

	clusterRoleBindingName := fmt.Sprintf("%v-%v-cluster-permissions", instance.Name, instance.Namespace)
	err = j.platformService.CreateUserClusterRoleBinding(instance, clusterRoleBindingName, edpJenkinsClusterRoleName)
	if err != nil {
		return &instance, errors.Wrapf(err, "Failed to create Cluster Role Binding %v", instance.Name)
	}

	err = j.platformService.CreatePersistentVolumeClaim(instance)
	if err != nil {
		return &instance, errors.Wrapf(err, "Failed to create Volume for %v", instance.Name)
	}

	err = j.platformService.CreateService(instance)
	if err != nil {
		return &instance, errors.Wrapf(err, "Failed to create Service for %v/%v", instance.Namespace, instance.Name)
	}

	err = j.platformService.CreateExternalEndpoint(instance)
	if err != nil {
		return &instance, errors.Wrap(err, "Failed to create Route.")
	}

	err = j.platformService.CreateDeployment(instance)
	if err != nil {
		return &instance, errors.Wrap(err, "Failed to create Deployment Config.")
	}

	return &instance, nil
}

// IsDeploymentConfigReady check if DC for Jenkins is ready
func (j JenkinsServiceImpl) IsDeploymentReady(instance v1alpha1.Jenkins) (bool, error) {
	return j.platformService.IsDeploymentReady(instance)
}
