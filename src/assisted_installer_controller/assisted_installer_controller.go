package assisted_installer_controller

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/openshift/assisted-installer/src/common"

	"github.com/openshift/assisted-installer/src/inventory_client"
	"github.com/openshift/assisted-installer/src/k8s_client"
	"github.com/openshift/assisted-installer/src/ops"
	"github.com/openshift/assisted-service/models"

	metal3v1alpha1 "github.com/metal3-io/baremetal-operator/pkg/apis/metal3/v1alpha1"
	"github.com/sirupsen/logrus"
	"k8s.io/api/certificates/v1beta1"
	certificatesv1beta1 "k8s.io/api/certificates/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	generalWaitTimeoutInt = 30
)

var GeneralWaitTimeout = generalWaitTimeoutInt * time.Second

// assisted installer controller is added to control installation process after  bootstrap pivot
// assisted installer will deploy it on installation process
// as a first step it will wait till nodes are added to cluster and update their status to Done

type ControllerConfig struct {
	ClusterID            string `envconfig:"CLUSTER_ID" required:"true" `
	URL                  string `envconfig:"INVENTORY_URL" required:"true"`
	PullSecretToken      string `envconfig:"PULL_SECRET_TOKEN" required:"true"`
	SkipCertVerification bool   `envconfig:"SKIP_CERT_VERIFICATION" required:"false" default:"false"`
	CACertPath           string `envconfig:"CA_CERT_PATH" required:"false" default:""`
}

type Controller interface {
	WaitAndUpdateNodesStatus()
}

type controller struct {
	ControllerConfig
	log *logrus.Logger
	ops ops.Ops
	ic  inventory_client.InventoryClient
	kc  k8s_client.K8SClient
}

func NewController(log *logrus.Logger, cfg ControllerConfig, ops ops.Ops, ic inventory_client.InventoryClient, kc k8s_client.K8SClient) *controller {
	return &controller{
		log:              log,
		ControllerConfig: cfg,
		ops:              ops,
		ic:               ic,
		kc:               kc,
	}
}

func (c *controller) WaitAndUpdateNodesStatus() {
	c.log.Infof("Waiting till all nodes will join and update status to assisted installer")
	ignoreStatuses := []string{models.HostStatusDisabled,
		models.HostStatusError, models.HostStatusInstalled}
	for {
		time.Sleep(GeneralWaitTimeout)
		assistedInstallerNodesMap, err := c.ic.GetHosts(ignoreStatuses)
		if err != nil {
			c.log.WithError(err).Error("Failed to get node map from inventory")
		}
		if len(assistedInstallerNodesMap) == 0 {
			break
		}
		c.log.Infof("Searching for host to change status")
		nodes, err := c.kc.ListNodes()
		if err != nil {
			continue
		}
		for _, node := range nodes.Items {
			host, ok := assistedInstallerNodesMap[node.Name]
			if !ok {
				continue
			}

			c.log.Infof("Found new joined node %s with inventory id %s, kubernetes id %s, updating its status to %s",
				node.Name, host.Host.ID.String(), node.Status.NodeInfo.SystemUUID, models.HostStageDone)
			if err := c.ic.UpdateHostInstallProgress(host.Host.ID.String(), models.HostStageDone, ""); err != nil {
				c.log.Errorf("Failed to update node %s installation status, %s", node.Name, err)
				continue
			}
		}
		c.updateConfiguringStatusIfNeeded(assistedInstallerNodesMap)

	}
	c.log.Infof("All nodes were found. WaitAndUpdateNodesStatus - Done")
}

func (c *controller) getMCSLogs() (string, error) {
	logs := ""
	namespace := "openshift-machine-config-operator"
	pods, err := c.kc.GetPods(namespace, map[string]string{"k8s-app": "machine-config-server"})
	if err != nil {
		c.log.WithError(err).Warnf("Failed to get mcs pods")
		return "", nil
	}
	for _, pod := range pods {
		podLogs, err := c.kc.GetPodLogs(namespace, pod.Name, generalWaitTimeoutInt*10)
		if err != nil {
			c.log.WithError(err).Warnf("Failed to get logs of pod %s", pod.Name)
			return "", nil
		}
		logs += podLogs
	}
	return logs, nil
}

func (c *controller) updateConfiguringStatusIfNeeded(hosts map[string]inventory_client.HostData) {
	logs, err := c.getMCSLogs()
	if err != nil {
		return
	}
	common.SetConfiguringStatusForHosts(c.ic, hosts, logs, true, c.log)
}

func (c *controller) ApproveCsrs(done <-chan bool, wg *sync.WaitGroup) {
	defer wg.Done()
	c.log.Infof("Start approving csrs")
	ticker := time.NewTicker(GeneralWaitTimeout)
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			csrs, err := c.kc.ListCsrs()
			if err != nil {
				continue
			}
			c.approveCsrs(csrs)
		}
	}
}

func (c controller) approveCsrs(csrs *v1beta1.CertificateSigningRequestList) {
	for i := range csrs.Items {
		csr := csrs.Items[i]
		if !isCsrApproved(&csr) {
			c.log.Infof("Approving csr %s", csr.Name)
			// We can fail and it is ok, we will retry on the next time
			_ = c.kc.ApproveCsr(&csr)
		}
	}
}

func isCsrApproved(csr *certificatesv1beta1.CertificateSigningRequest) bool {
	for _, condition := range csr.Status.Conditions {
		if condition.Type == certificatesv1beta1.CertificateApproved {
			return true
		}
	}
	return false
}

func (c controller) PostInstallConfigs(wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		time.Sleep(GeneralWaitTimeout)
		cluster, err := c.ic.GetCluster()
		if err != nil {
			c.log.WithError(err).Errorf("Failed to get cluster %s from assisted-service", c.ClusterID)
			continue
		}
		// waiting till cluster will be installed(3 masters must be installed)
		if *cluster.Status != models.ClusterStatusFinalizing {
			continue
		}
		break
	}
	c.addRouterCAToClusterCA()
	c.unpatchEtcd()
	c.waitForConsole()
	c.sendCompleteInstallation(true, "")
}

func (c controller) UpdateBMHs(wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		time.Sleep(GeneralWaitTimeout)
		exists, err := c.kc.IsMetalProvisioningExists()
		if err != nil {
			continue
		}
		if err == nil && exists {
			c.log.Infof("Provisioning CR exists, no need to update BMHs")
			return
		}

		bmhs, err := c.kc.ListBMHs()
		if err != nil {
			c.log.WithError(err).Errorf("Failed to BMH hosts")
			continue
		}

		allUpdated := c.updateBMHStatus(bmhs)
		if allUpdated {
			c.log.Infof("Updated all the BMH CRs, finished successfully")
			return
		}
	}
}

func (c controller) updateBMHStatus(bmhList metal3v1alpha1.BareMetalHostList) bool {
	allUpdated := true
	for i := range bmhList.Items {
		bmh := bmhList.Items[i]
		c.log.Infof("Checking bmh %s", bmh.Name)
		annotations := bmh.GetAnnotations()
		content := []byte(annotations[metal3v1alpha1.StatusAnnotation])
		if annotations[metal3v1alpha1.StatusAnnotation] == "" {
			c.log.Infof("Skipping setting status of BMH host %s, status annotation not present", bmh.Name)
			continue
		}
		allUpdated = false
		objStatus, err := c.unmarshalStatusAnnotation(content)
		if err != nil {
			c.log.WithError(err).Errorf("Failed to unmarshal status annotation of %s", bmh.Name)
			continue
		}
		bmh.Status = *objStatus
		if bmh.Status.LastUpdated.IsZero() {
			// Ensure the LastUpdated timestamp in set to avoid
			// infinite loops if the annotation only contained
			// part of the status information.
			t := metav1.Now()
			bmh.Status.LastUpdated = &t
		}
		err = c.kc.UpdateBMHStatus(&bmh)
		if err != nil {
			c.log.WithError(err).Errorf("Failed to update status of BMH %s", bmh.Name)
			continue
		}
		delete(annotations, metal3v1alpha1.StatusAnnotation)
		err = c.kc.UpdateBMH(&bmh)
		if err != nil {
			c.log.WithError(err).Errorf("Failed to remove status annotation from BMH %s", bmh.Name)
		}
	}
	return allUpdated
}

func (c controller) unmarshalStatusAnnotation(content []byte) (*metal3v1alpha1.BareMetalHostStatus, error) {
	bmhStatus := &metal3v1alpha1.BareMetalHostStatus{}
	err := json.Unmarshal(content, bmhStatus)
	if err != nil {
		return nil, err
	}
	return bmhStatus, nil
}

func (c controller) unpatchEtcd() {
	c.log.Infof("Unpatching etcd")
	for {
		if err := c.kc.UnPatchEtcd(); err != nil {
			c.log.Error(err)
			continue
		}
		break
	}

}

// AddRouterCAToClusterCA adds router CA to cluster CA in kubeconfig
func (c controller) addRouterCAToClusterCA() {
	cmName := "default-ingress-cert"
	cmNamespace := "openshift-config-managed"
	c.log.Infof("Start adding ingress ca to cluster")
	for {
		caConfigMap, err := c.kc.GetConfigMap(cmNamespace, cmName)

		if err != nil {
			c.log.WithError(err).Errorf("fetching %s configmap from %s namespace", cmName, cmNamespace)
			continue
		}

		c.log.Infof("Sending ingress certificate to inventory service. Certificate data %s", caConfigMap.Data["ca-bundle.crt"])
		err = c.ic.UploadIngressCa(caConfigMap.Data["ca-bundle.crt"], c.ClusterID)
		if err != nil {
			c.log.WithError(err).Errorf("Failed to upload ingress ca to assisted-service")
			continue
		}
		c.log.Infof("Ingress ca successfully sent to inventory")
		return
	}
}

func (c controller) waitForConsole() {
	c.log.Infof("Waiting for console pod")

	// TODO maybe need some timeout?
	for {
		pods, err := c.kc.GetPods("openshift-console", map[string]string{"app": "console", "component": "ui"})
		if err != nil {
			c.log.WithError(err).Warnf("Failed to get console pods")
			continue
		}
		for _, pod := range pods {
			if pod.Status.Phase == "Running" {
				c.log.Infof("Found running console pod")
				return
			}
		}
	}
}

func (c controller) sendCompleteInstallation(isSuccess bool, errorInfo string) {
	c.log.Infof("Start complete installation step")
	for {
		if err := c.ic.CompleteInstallation(c.ClusterID, isSuccess, errorInfo); err != nil {
			c.log.Error(err)
			continue
		}
		break
	}
	c.log.Infof("Done complete installation step")
}
