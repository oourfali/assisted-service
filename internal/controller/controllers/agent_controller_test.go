package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/swag"
	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	common_api "github.com/openshift/assisted-service/api/common"
	hiveext "github.com/openshift/assisted-service/api/hiveextension/v1beta1"
	"github.com/openshift/assisted-service/api/v1beta1"
	"github.com/openshift/assisted-service/internal/bminventory"
	"github.com/openshift/assisted-service/internal/common"
	"github.com/openshift/assisted-service/internal/gencrypto"
	"github.com/openshift/assisted-service/models"
	"github.com/openshift/assisted-service/restapi/operations/installer"
	conditionsv1 "github.com/openshift/custom-resource-status/conditions/v1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newHostRequest(host *v1beta1.Agent) ctrl.Request {
	namespacedName := types.NamespacedName{
		Namespace: host.ObjectMeta.Namespace,
		Name:      host.ObjectMeta.Name,
	}
	return ctrl.Request{NamespacedName: namespacedName}
}

func newAgent(name, namespace string, spec v1beta1.AgentSpec) *v1beta1.Agent {
	return &v1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Finalizers: []string{AgentFinalizerName}, // adding finalizer to avoid reconciling twice in the unit tests
		},
		Spec: spec,
	}
}

var _ = Describe("agent reconcile", func() {
	var (
		c                               client.Client
		hr                              *AgentReconciler
		ctx                             = context.Background()
		mockCtrl                        *gomock.Controller
		mockInstallerInternal           *bminventory.MockInstallerInternals
		sId                             strfmt.UUID
		backEndCluster                  *common.Cluster
		ignitionEndpointTokenSecretName = "ignition-endpoint-secret"
	)

	BeforeEach(func() {
		c = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		mockCtrl = gomock.NewController(GinkgoT())
		mockInstallerInternal = bminventory.NewMockInstallerInternals(mockCtrl)

		hr = &AgentReconciler{
			Client:    c,
			APIReader: c,
			Scheme:    scheme.Scheme,
			Log:       common.GetTestLog(),
			Installer: mockInstallerInternal,
		}
		sId = strfmt.UUID(uuid.New().String())
		backEndCluster = &common.Cluster{Cluster: models.Cluster{ID: &sId}}
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	It("none existing agent", func() {
		host := newAgent("host", testNamespace, v1beta1.AgentSpec{})
		Expect(c.Create(ctx, host)).To(BeNil())

		noneExistingHost := newAgent("host2", testNamespace, v1beta1.AgentSpec{})

		result, err := hr.Reconcile(ctx, newHostRequest(noneExistingHost))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	It("no other updates after finalizer was added", func() {
		host := newAgent("host", testNamespace, v1beta1.AgentSpec{})
		host.ObjectMeta.Finalizers = nil
		Expect(c.Create(ctx, host)).To(BeNil())

		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{Requeue: true}))

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      "host",
		}
		Expect(c.Get(ctx, key, host)).To(BeNil())
		Expect(len(host.Status.Conditions)).To(Equal(0))
	})

	It("cluster deployment not set", func() {
		hostId := strfmt.UUID(uuid.New().String())
		infraEnvId := strfmt.UUID(uuid.New().String())
		host := newAgent("host", testNamespace, v1beta1.AgentSpec{})
		Expect(c.Create(ctx, host)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(&common.Host{Host: models.Host{ID: &hostId, InfraEnvID: infraEnvId}}, nil).AnyTimes()
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      "host",
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
	})

	It("cluster deployment not found", func() {
		host := newAgent("host", testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		Expect(c.Create(ctx, host)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(&common.Host{}, nil).AnyTimes()
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      "host",
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.BackendErrorReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
	})

	It("cluster not found in database", func() {
		host := newAgent("host", testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		Expect(c.Create(ctx, host)).To(BeNil())
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(&common.Host{}, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(nil, gorm.ErrRecordNotFound).Times(1)
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      "host",
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		expectedState := fmt.Sprintf("%s %s", v1beta1.BackendErrorMsg, gorm.ErrRecordNotFound)
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(expectedState))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.BackendErrorReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
	})

	It("error getting cluster from database", func() {
		host := newAgent("host", testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		Expect(c.Create(ctx, host)).To(BeNil())
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		errString := "Error getting Cluster"
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(&common.Host{}, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(nil, common.NewApiError(http.StatusInternalServerError,
			errors.New(errString))).Times(1)
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      "host",
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		expectedState := fmt.Sprintf("%s %s", v1beta1.BackendErrorMsg, errString)
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(expectedState))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.BackendErrorReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
	})

	It("host not found ", func() {
		host := newAgent("host", testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(nil, gorm.ErrRecordNotFound).Times(1)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(0)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      "host",
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(agent.ObjectMeta.DeletionTimestamp.IsZero()).To(BeFalse())
	})

	It("Agent ValidationInfo update", func() {
		hostId := strfmt.UUID(uuid.New().String())
		infraEnvId := strfmt.UUID(uuid.New().String())

		validationInfoKey := "some-check"
		var validationInfoId = "checking1"

		validationInfo := common_api.ValidationsStatus{
			validationInfoKey: common_api.ValidationResults{
				{
					ID:      validationInfoId,
					Status:  "success",
					Message: "check1 is okay",
				},
			},
		}
		var bytesValidationInfo []byte
		var err error
		bytesValidationInfo, err = json.Marshal(validationInfo)
		Expect(err).To(BeNil())
		stringifiedValidationInfo := string(bytesValidationInfo)

		commonHost := &common.Host{
			Host: models.Host{
				ID:              &hostId,
				ClusterID:       &sId,
				Inventory:       common.GenerateTestDefaultInventory(),
				Status:          swag.String(models.HostStatusKnown),
				StatusInfo:      swag.String("Some status info"),
				InfraEnvID:      infraEnvId,
				ValidationsInfo: stringifiedValidationInfo,
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}

		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())

		Expect(c.Create(ctx, host)).To(BeNil())

		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)

		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(agent.Status.ValidationsInfo).ToNot(BeNil())
		Expect(agent.Status.ValidationsInfo[validationInfoKey]).ToNot(BeNil())
		Expect(len(agent.Status.ValidationsInfo[validationInfoKey])).To(Equal(1))
		Expect(agent.Status.ValidationsInfo[validationInfoKey][0].ID).To(Equal(validationInfoId))
	})

	It("Agent update", func() {
		mockClient := NewMockK8sClient(mockCtrl)
		hr.Client = mockClient
		newHostName := "hostname123"
		newRole := "worker"
		newInstallDiskPath := "/dev/disk/by-id/wwn-0x6141877064533b0020adf3bb03167694"
		hostId := strfmt.UUID(uuid.New().String())
		infraEnvId := strfmt.UUID(uuid.New().String())
		beforeUpdateHost := &common.Host{
			Host: models.Host{
				ID:         &hostId,
				ClusterID:  &sId,
				Inventory:  common.GenerateTestDefaultInventory(),
				Status:     swag.String(models.HostStatusInsufficient),
				StatusInfo: swag.String("I am insufficient"),
				InfraEnvID: infraEnvId,
			},
		}
		afterUpdateHost := &common.Host{
			Host: models.Host{
				ID:         &hostId,
				ClusterID:  &sId,
				Inventory:  common.GenerateTestDefaultInventory(),
				Status:     swag.String(models.HostStatusKnown),
				StatusInfo: swag.String("I am known"),
				InfraEnvID: infraEnvId,
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&beforeUpdateHost.Host,
			}}}
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		host.Spec.Hostname = newHostName
		host.Spec.Role = models.HostRole(newRole)
		host.Spec.InstallationDiskID = newInstallDiskPath
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(beforeUpdateHost, nil).Times(1)
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(afterUpdateHost, nil).Times(1)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(2)
		mockInstallerInternal.EXPECT().V2UpdateHostInternal(gomock.Any(), gomock.Any()).
			Do(func(ctx context.Context, param installer.V2UpdateHostParams) {
				Expect(param.HostUpdateParams.DisksSelectedConfig[0].ID).To(Equal(&newInstallDiskPath))
				Expect(param.HostUpdateParams.DisksSelectedConfig[0].Role).To(Equal(models.DiskRoleInstall))
				Expect(swag.StringValue(param.HostUpdateParams.HostName)).To(Equal(newHostName))
				Expect(param.HostID).To(Equal(hostId))
				Expect(param.InfraEnvID).To(Equal(infraEnvId))
				Expect(param.HostUpdateParams.HostRole).To(Equal(&newRole))
			}).Return(afterUpdateHost, nil).Times(2)
		Expect(c.Create(ctx, host)).To(BeNil())
		mockClient.EXPECT().Get(gomock.Any(), gomock.AssignableToTypeOf(types.NamespacedName{}), gomock.AssignableToTypeOf(&v1beta1.Agent{})).DoAndReturn(
			func(ctx context.Context, name types.NamespacedName, agent *v1beta1.Agent) error {
				return c.Get(ctx, name, agent)
			},
		).Times(3)
		mockClient.EXPECT().Get(gomock.Any(), gomock.AssignableToTypeOf(types.NamespacedName{}), gomock.AssignableToTypeOf(&hivev1.ClusterDeployment{})).DoAndReturn(
			func(ctx context.Context, name types.NamespacedName, cd *hivev1.ClusterDeployment) error {
				return c.Get(ctx, name, cd)
			},
		).Times(2)
		mockClient.EXPECT().Update(gomock.Any(), gomock.AssignableToTypeOf(&v1beta1.Agent{})).DoAndReturn(
			func(ctx context.Context, agent *v1beta1.Agent, opts ...client.UpdateOption) error {
				return c.Update(ctx, agent)
			},
		).Times(1)
		mockClient.EXPECT().Status().Return(mockClient).Times(1)
		mockClient.EXPECT().Update(gomock.Any(), gomock.AssignableToTypeOf(&v1beta1.Agent{})).DoAndReturn(
			func(ctx context.Context, agent *v1beta1.Agent, opts ...client.UpdateOption) error {
				return c.Status().Update(ctx, agent)
			},
		).Times(1)

		// We test 2 times to verify that agent is not updated the second time
		for i := 0; i != 2; i++ {
			result, err := hr.Reconcile(ctx, newHostRequest(host))
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
			agent := &v1beta1.Agent{}

			key := types.NamespacedName{
				Namespace: testNamespace,
				Name:      hostId.String(),
			}
			Expect(c.Get(ctx, key, agent)).To(BeNil())
			Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
			Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
			Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))
			Expect(agent.Status.DebugInfo.State).To(Equal(models.HostStatusKnown))
			Expect(agent.Status.DebugInfo.StateInfo).To(Equal("I am known"))
		}
	})

	It("Ignition endpoint is parsed correctly", func() {
		hostId := strfmt.UUID(uuid.New().String())
		infraEnvId := strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:         &hostId,
				ClusterID:  &sId,
				Inventory:  common.GenerateTestDefaultInventory(),
				Status:     swag.String(models.HostStatusKnown),
				StatusInfo: swag.String("Some status info"),
				InfraEnvID: infraEnvId,
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}

		host := newAgent("host", testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		ignitionEndpointTokenSecret := newSecret(ignitionEndpointTokenSecretName, host.Namespace, map[string][]byte{
			common.IgnitionTokenKeyInSecret: []byte("token"),
		})
		Expect(c.Create(ctx, ignitionEndpointTokenSecret)).To(BeNil())
		host.Spec.IgnitionEndpointTokenReference = &v1beta1.IgnitionEndpointTokenReference{
			Namespace: host.Namespace,
			Name:      ignitionEndpointTokenSecret.Name,
		}
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())

		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
		mockInstallerInternal.EXPECT().V2UpdateHostInternal(gomock.Any(), gomock.Any()).Return(commonHost, nil).Times(1)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      "host",
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
	})

	It("Agent update empty disk path", func() {
		newInstallDiskPath := ""
		hostId := strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:                 &hostId,
				ClusterID:          &sId,
				Inventory:          common.GenerateTestDefaultInventory(),
				InstallationDiskID: "/dev/disk/by-id/wwn-0x1111111111111111111111",
				Status:             swag.String(models.HostStatusKnown),
				StatusInfo:         swag.String("Some status info"),
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		host.Spec.InstallationDiskID = newInstallDiskPath
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
		mockInstallerInternal.EXPECT().V2UpdateHostInternal(gomock.Any(), gomock.Any()).Return(nil, nil).Times(0)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))
	})

	It("Host parameters are not updated post install", func() {
		newRole := "worker"
		hostId := strfmt.UUID(uuid.New().String())
		status := models.HostStatusPreparingForInstallation
		commonHost := &common.Host{
			Host: models.Host{
				ID:        &hostId,
				ClusterID: &sId,
				Status:    &status,
				Role:      models.HostRoleMaster,
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		host.Spec.Hostname = "newhostname"
		host.Spec.Role = models.HostRole(newRole)
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
		mockInstallerInternal.EXPECT().V2UpdateHostInternal(gomock.Any(), gomock.Any()).Return(nil, nil).Times(0)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))
	})

	It("Agent update error", func() {
		newHostName := "hostname123"
		newRole := "worker"
		status := models.HostStatusKnown
		hostId := strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:        &hostId,
				ClusterID: &sId,
				Status:    &status,
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		host.Spec.Hostname = newHostName
		host.Spec.Role = models.HostRole(newRole)
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
		errString := "update internal error"
		mockInstallerInternal.EXPECT().V2UpdateHostInternal(gomock.Any(), gomock.Any()).Return(nil, common.NewApiError(http.StatusInternalServerError,
			errors.New(errString)))
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		expectedState := fmt.Sprintf("%s %s", v1beta1.BackendErrorMsg, errString)
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(expectedState))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.BackendErrorReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.InstalledCondition).Status).To(Equal(corev1.ConditionFalse))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.RequirementsMetCondition).Status).To(Equal(corev1.ConditionFalse))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.ValidatedCondition).Status).To(Equal(corev1.ConditionUnknown))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.ConnectedCondition).Status).To(Equal(corev1.ConditionTrue))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.ConnectedCondition).Message).To(Equal(v1beta1.AgentConnectedMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.ConnectedCondition).Reason).To(Equal(v1beta1.AgentConnectedReason))
	})

	It("Agent update approved", func() {
		hostId := strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:         &hostId,
				ClusterID:  &sId,
				Inventory:  common.GenerateTestDefaultInventory(),
				Status:     swag.String(models.HostStatusKnown),
				StatusInfo: swag.String("Some status info"),
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}

		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		host.Spec.Approved = true
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
		mockInstallerInternal.EXPECT().UpdateHostApprovedInternal(gomock.Any(), gomock.Any(), gomock.Any(), true).Return(nil)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))
	})

	It("Agent unbind", func() {
		hostId := strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:         &hostId,
				ClusterID:  &sId,
				Inventory:  common.GenerateTestDefaultInventory(),
				Status:     swag.String(models.HostStatusKnown),
				StatusInfo: swag.String("Some status info"),
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}

		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		host.Spec.ClusterDeploymentName = nil
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil)
		mockInstallerInternal.EXPECT().UnbindHostInternal(gomock.Any(), gomock.Any()).Return(commonHost, nil)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))
	})

	It("Agent status update does not fail when unbind fails", func() {
		hostId := strfmt.UUID(uuid.New().String())
		logCollectionTime, _ := strfmt.ParseDateTime("2022-02-17T21:41:51Z")
		commonHost := &common.Host{
			Host: models.Host{
				ID:              &hostId,
				ClusterID:       &sId,
				Inventory:       common.GenerateTestDefaultInventory(),
				Status:          swag.String(models.HostStatusKnown),
				StatusInfo:      swag.String("Some status info"),
				LogsCollectedAt: logCollectionTime,
			},
		}
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		host.Spec.ClusterDeploymentName = nil
		Expect(c.Create(ctx, host)).To(BeNil())
		errString := "failed to find host in infraEnv"
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		mockInstallerInternal.EXPECT().UnbindHostInternal(gomock.Any(), gomock.Any()).Return(commonHost, common.NewApiError(http.StatusNotFound, errors.New(errString)))
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	It("Agent bind", func() {
		hostId := strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:         &hostId,
				Inventory:  common.GenerateTestDefaultInventory(),
				Status:     swag.String(models.HostStatusKnown),
				StatusInfo: swag.String("Some status info"),
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID:    &sId,
			Hosts: []*models.Host{}}}

		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
		mockInstallerInternal.EXPECT().BindHostInternal(gomock.Any(), gomock.Any()).Return(commonHost, nil)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))
	})

	It("Agent bind, cluster not found in DB and recover", func() {
		hostId := strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:         &hostId,
				Inventory:  common.GenerateTestDefaultInventory(),
				Status:     swag.String(models.HostStatusKnown),
				StatusInfo: swag.String("Some status info"),
			},
		}

		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(nil, gorm.ErrRecordNotFound).Times(1)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		expectedState := fmt.Sprintf("%s %s", v1beta1.BackendErrorMsg, gorm.ErrRecordNotFound)
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(expectedState))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.BackendErrorReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))

		By("Reconcile again with existing cluster")
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
		mockInstallerInternal.EXPECT().BindHostInternal(gomock.Any(), gomock.Any()).Return(commonHost, nil).Times(1)
		result, err = hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))

	})

	It("Move Agent- unbind", func() {
		hostId := strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:         &hostId,
				ClusterID:  &sId,
				Inventory:  common.GenerateTestDefaultInventory(),
				Status:     swag.String(models.HostStatusKnown),
				StatusInfo: swag.String("Some status info"),
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		targetId := strfmt.UUID(uuid.New().String())
		targetBECluster := &common.Cluster{Cluster: models.Cluster{
			ID: &targetId}}
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(targetBECluster, nil)
		mockInstallerInternal.EXPECT().UnbindHostInternal(gomock.Any(), gomock.Any()).Return(commonHost, nil)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))
	})

	It("validate Event URL", func() {
		_, priv, err := gencrypto.ECDSAKeyPairPEM()
		Expect(err).NotTo(HaveOccurred())
		os.Setenv("EC_PRIVATE_KEY_PEM", priv)
		defer os.Unsetenv("EC_PRIVATE_KEY_PEM")
		Expect(err).NotTo(HaveOccurred())
		serviceBaseURL := "http://acme.com"
		hr.ServiceBaseURL = serviceBaseURL
		hostId := strfmt.UUID(uuid.New().String())
		expectedEventUrlPrefix := fmt.Sprintf("%s/api/assisted-install/v2/events?host_id=%s", serviceBaseURL, hostId.String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:         &hostId,
				ClusterID:  &sId,
				Status:     swag.String(models.HostStatusKnown),
				StatusInfo: swag.String("Some status info"),
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())

		Expect(agent.Status.DebugInfo.EventsURL).NotTo(BeEmpty())
		Expect(agent.Status.DebugInfo.EventsURL).To(HavePrefix(expectedEventUrlPrefix))
	})

	It("validate Logs URL", func() {
		serviceBaseURL := "http://example.com"
		hr.ServiceBaseURL = serviceBaseURL
		hostId := strfmt.UUID(uuid.New().String())
		expectedLogsUrlPrefix := fmt.Sprintf("%s/api/assisted-install/v2/clusters/%s/logs", serviceBaseURL, sId.String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:         &hostId,
				ClusterID:  &sId,
				Status:     swag.String(models.HostStatusKnown),
				StatusInfo: swag.String("Some status info"),
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		By("before installation")
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(Succeed())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
		Expect(c.Create(ctx, host)).To(Succeed())
		request := newHostRequest(host)
		result, err := hr.Reconcile(ctx, request)
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}
		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(Succeed())
		Expect(agent.Status.DebugInfo.LogsURL).To(Equal(""))

		By("during installation")
		backEndCluster.Hosts[0].Status = swag.String(models.HostStatusInstalling)
		backEndCluster.Hosts[0].LogsCollectedAt = strfmt.DateTime(time.Now())
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
		_, err = hr.Reconcile(ctx, request)
		Expect(err).To(BeNil())
		Expect(c.Get(ctx, key, agent)).To(Succeed())
		Expect(agent.Status.DebugInfo.LogsURL).To(HavePrefix(expectedLogsUrlPrefix))

		By("after installation")
		backEndCluster.Hosts[0].Status = swag.String(models.HostStatusInstalled)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
		_, err = hr.Reconcile(ctx, request)
		Expect(err).To(BeNil())
		Expect(c.Get(ctx, key, agent)).To(Succeed())
		Expect(agent.Status.DebugInfo.LogsURL).To(HavePrefix(expectedLogsUrlPrefix))
	})

	It("Agent update ignition override valid cases", func() {
		hostId := strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:        &hostId,
				ClusterID: &sId,
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}

		ignitionConfigOverrides := `{"ignition": {"version": "3.1.0"}, "storage": {"files": [{"path": "/tmp/example", "contents": {"source": "data:text/plain;base64,aGVscGltdHJhcHBlZGluYXN3YWdnZXJzcGVj"}}]}}`
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)

		By("Reconcile without setting ignition override, validate update ignition override didn't run")
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))

		By("Reconcile add update ignition override, validate UpdateHostIgnitionInternal run once")
		mockInstallerInternal.EXPECT().V2UpdateHostIgnitionInternal(gomock.Any(), gomock.Any()).Return(nil, nil)

		Expect(c.Get(ctx, key, agent)).To(BeNil())
		agent.Spec.IgnitionConfigOverrides = ignitionConfigOverrides
		Expect(c.Update(ctx, agent)).To(BeNil())
		result, err = hr.Reconcile(ctx, newHostRequest(agent))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(agent.Spec.IgnitionConfigOverrides).To(Equal(ignitionConfigOverrides))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))

	})

	It("Agent update ignition config errors", func() {
		hostId := strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:        &hostId,
				ClusterID: &sId,
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}

		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())

		By("Reconcile with ignition config, UpdateHostIgnitionInternal returns error")
		ignitionConfigOverrides := `{"ignition": "version": "3.1.0"}, "storage": {"files": [{"path": "/tmp/example", "contents": {"source": "data:text/plain;base64,aGVscGltdHJhcHBlZGluYXN3YWdnZXJzcGVj"}}]}}`
		errString := "update internal error"
		mockInstallerInternal.EXPECT().V2UpdateHostIgnitionInternal(gomock.Any(), gomock.Any()).Return(nil, errors.Errorf(errString)).Times(1)
		host.Spec.IgnitionConfigOverrides = ignitionConfigOverrides
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))
		Expect(c.Get(ctx, key, host)).To(BeNil())
		expectedState := fmt.Sprintf("%s %s", v1beta1.BackendErrorMsg, errString)
		Expect(conditionsv1.FindStatusCondition(host.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(expectedState))
		Expect(conditionsv1.FindStatusCondition(host.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.BackendErrorReason))
		Expect(conditionsv1.FindStatusCondition(host.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
	})

	It("Agent update installer args valid cases", func() {
		hostId := strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:        &hostId,
				ClusterID: &sId,
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}

		installerArgs := `["--append-karg", "ip=192.0.2.2::192.0.2.254:255.255.255.0:core0.example.com:enp1s0:none", "--save-partindex", "1", "-n"]`
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)

		By("Reconcile without setting args, validate update installer args didn't run")
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))

		By("Reconcile add update installer args, validate UpdateHostInstallerArgsInternal run once")
		mockInstallerInternal.EXPECT().V2UpdateHostInstallerArgsInternal(gomock.Any(), gomock.Any()).Return(nil, nil)
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)

		Expect(c.Get(ctx, key, agent)).To(BeNil())
		agent.Spec.InstallerArgs = installerArgs
		Expect(c.Update(ctx, agent)).To(BeNil())
		result, err = hr.Reconcile(ctx, newHostRequest(agent))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(agent.Spec.InstallerArgs).To(Equal(installerArgs))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))

		By("Reconcile with same installer args, validate UpdateHostInstallerArgsInternal didn't run")
		var j []string
		err = json.Unmarshal([]byte(installerArgs), &j)
		Expect(err).To(BeNil())
		arrBytes, _ := json.Marshal(j)
		commonHostUpdated := &common.Host{
			Host: models.Host{
				ID:            &hostId,
				ClusterID:     &sId,
				InstallerArgs: string(arrBytes),
			},
		}
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHostUpdated, nil)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(agent.Spec.InstallerArgs).To(Equal(installerArgs))
		result, err = hr.Reconcile(ctx, newHostRequest(agent))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))
	})

	It("Agent update installer args errors", func() {

		hostId := strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:        &hostId,
				ClusterID: &sId,
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}

		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).AnyTimes()
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())

		By("Reconcile with bad json in installer args, validate UpdateHostInstallerArgsInternal didn't run")
		installerArgs := `"--append-karg", "ip=192.0.2.2::192.0.2.254:255.255.255.0:core0.example.com:enp1s0:none", "--save-partindex", "1", "-n"]`
		host.Spec.InstallerArgs = installerArgs
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{Requeue: false}))
		Expect(c.Get(ctx, key, host)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(host.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.InputErrorReason))
		Expect(conditionsv1.FindStatusCondition(host.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))

		By("Reconcile with installer args, UpdateHostInstallerArgsInternal returns error")
		installerArgs = `["--append-karg", "ip=192.0.2.2::192.0.2.254:255.255.255.0:core0.example.com:enp1s0:none", "--save-partindex", "1", "-n"]`
		errString := "update internal error"
		mockInstallerInternal.EXPECT().V2UpdateHostInstallerArgsInternal(gomock.Any(), gomock.Any()).Return(nil, errors.Errorf(errString)).Times(1)
		host.Spec.InstallerArgs = installerArgs
		Expect(c.Update(ctx, host)).To(BeNil())
		result, err = hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))
		Expect(c.Get(ctx, key, host)).To(BeNil())
		expectedState := fmt.Sprintf("%s %s", v1beta1.BackendErrorMsg, errString)
		Expect(conditionsv1.FindStatusCondition(host.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(expectedState))
		Expect(conditionsv1.FindStatusCondition(host.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.BackendErrorReason))
		Expect(conditionsv1.FindStatusCondition(host.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
	})

	It("Agent inventory status", func() {
		macAddress := "some MAC address"
		hostId := strfmt.UUID(uuid.New().String())
		inventory := models.Inventory{
			CPU: &models.CPU{
				Architecture: common.DefaultCPUArchitecture,
				Flags:        []string{"vmx"},
			},
			SystemVendor: &models.SystemVendor{
				Manufacturer: "Red Hat",
				ProductName:  "-bad-label-name",
				Virtual:      true,
			},
			Interfaces: []*models.Interface{
				{
					Name: "eth0",
					IPV4Addresses: []string{
						"1.2.3.4/24",
					},
					IPV6Addresses: []string{
						"1001:db8::10/120",
					},
					MacAddress: macAddress,
				},
			},
			Disks: []*models.Disk{
				{Path: "/dev/sda", Bootable: true, DriveType: "HDD"},
				{Path: "/dev/sdb", Bootable: false, DriveType: "HDD"},
			},
		}
		inv, _ := json.Marshal(&inventory)
		commonHost := &common.Host{
			Host: models.Host{
				ID:         &hostId,
				ClusterID:  &sId,
				Inventory:  string(inv),
				Status:     swag.String(models.HostStatusKnown),
				StatusInfo: swag.String("Some status info"),
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(2)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))
		Expect(agent.Status.Inventory.Interfaces[0].MacAddress).To(Equal(macAddress))
		Expect(agent.GetAnnotations()[InventoryLabelPrefix+"version"]).To(Equal("0.1"))
		Expect(agent.GetLabels()[InventoryLabelPrefix+"storage-hasnonrotationaldisk"]).To(Equal("false"))
		Expect(agent.GetLabels()[InventoryLabelPrefix+"cpu-architecture"]).To(Equal(common.DefaultCPUArchitecture))
		Expect(agent.GetLabels()[InventoryLabelPrefix+"cpu-virtenabled"]).To(Equal("true"))
		Expect(agent.GetLabels()[InventoryLabelPrefix+"host-manufacturer"]).To(Equal("RedHat"))
		Expect(agent.GetLabels()[InventoryLabelPrefix+"host-productname"]).To(Equal(""))
		Expect(agent.GetLabels()[InventoryLabelPrefix+"host-isvirtual"]).To(Equal("true"))

		result, err = hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	It("Agent ntp sources, role, bootstrap status", func() {
		hostId := strfmt.UUID(uuid.New().String())
		srcName := "1.1.1.1"
		srcState := models.SourceStateError
		role := models.HostRoleMaster
		bootStrap := true
		ntpSources := []*models.NtpSource{
			{
				SourceName:  srcName,
				SourceState: srcState,
			},
		}
		ntp, _ := json.Marshal(&ntpSources)
		commonHost := &common.Host{
			Host: models.Host{
				ID:         &hostId,
				ClusterID:  &sId,
				Role:       role,
				Bootstrap:  bootStrap,
				NtpSources: string(ntp),
				Status:     swag.String(models.HostStatusKnown),
				StatusInfo: swag.String("Some status info"),
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).Times(1)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))
		Expect(agent.Status.NtpSources[0].SourceName).To(Equal(srcName))
		Expect(agent.Status.NtpSources[0].SourceState).To(Equal(srcState))
		Expect(agent.Status.Role).To(Equal(role))
		Expect(agent.Status.Bootstrap).To(Equal(bootStrap))
	})

	It("Agent auto-assign to master role status", func() {
		hostId := strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:            &hostId,
				ClusterID:     &sId,
				Role:          models.HostRoleAutoAssign,
				SuggestedRole: models.HostRoleMaster,
				Status:        swag.String(models.HostStatusKnown),
				StatusInfo:    swag.String("Some status info"),
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).Times(1)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Message).To(Equal(v1beta1.SyncedOkMsg))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Reason).To(Equal(v1beta1.SyncedOkReason))
		Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, v1beta1.SpecSyncedCondition).Status).To(Equal(corev1.ConditionTrue))
		Expect(agent.Status.Role).To(Equal(models.HostRoleMaster))
	})

	It("Agent progress status", func() {
		hostId := strfmt.UUID(uuid.New().String())
		progress := &models.HostProgressInfo{
			CurrentStage:   models.HostStageConfiguring,
			ProgressInfo:   "some info",
			StageStartedAt: strfmt.DateTime(time.Now()),
			StageUpdatedAt: strfmt.DateTime(time.Now()),
		}
		commonHost := &common.Host{
			Host: models.Host{
				ID:         &hostId,
				ClusterID:  &sId,
				Inventory:  common.GenerateTestDefaultInventory(),
				Status:     swag.String(models.HostStatusKnown),
				StatusInfo: swag.String("Some status info"),
				Progress:   progress,
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(2)
		Expect(c.Create(ctx, host)).To(BeNil())
		result, err := hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent := &v1beta1.Agent{}

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(agent.Status.Progress.ProgressInfo).To(Equal(progress.ProgressInfo))
		Expect(agent.Status.Progress.StageStartTime).NotTo(BeNil())
		Expect(agent.Status.Progress.StageUpdateTime).NotTo(BeNil())

		// Reset progress
		commonHost.Progress = nil
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil)
		result, err = hr.Reconcile(ctx, newHostRequest(host))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		agent = &v1beta1.Agent{}
		Expect(c.Get(ctx, key, agent)).To(BeNil())
		Expect(agent.Status.Progress.ProgressInfo).To(BeEmpty())
		Expect(agent.Status.Progress.CurrentStage).To(BeEmpty())
		Expect(agent.Status.Progress.StageStartTime).To(BeNil())
		Expect(agent.Status.Progress.StageUpdateTime).To(BeNil())

	})

})

type notFoundError struct{}

func (n *notFoundError) Status() metav1.Status {
	return metav1.Status{
		Reason: metav1.StatusReasonNotFound,
	}
}

func (n *notFoundError) Error() string {
	return "Stam"
}

var _ = Describe("Approve CSRs", func() {
	/* Decoded output of PEM formatted client CSR

	Certificate Request:
	    Data:
	        Version: 1 (0x0)
	        Subject: O = system:nodes, CN = system:node:ostest-extraworker-3
	        Subject Public Key Info:
	            Public Key Algorithm: id-ecPublicKey
	                Public-Key: (256 bit)
	                pub:
	                    04:f3:d3:02:4d:a3:b4:33:47:94:54:20:36:e4:e0:
	                    60:53:46:50:33:71:3d:17:2d:a8:d0:c9:c9:22:5d:
	                    08:f1:a3:02:08:06:ec:a6:05:44:57:40:d0:96:18:
	                    b1:d6:08:51:30:00:2f:79:c0:36:47:65:02:6f:c4:
	                    67:52:14:bf:60
	                ASN1 OID: prime256v1
	                NIST CURVE: P-256
	        Attributes:
	            a0:00
	    Signature Algorithm: ecdsa-with-SHA256
	         30:44:02:20:13:06:4d:20:bf:21:d6:e0:9f:e7:fd:5b:e6:58:
	         06:cf:32:2f:3b:63:82:fb:89:d2:f0:99:6a:2b:c2:84:87:84:
	         02:20:59:76:4d:c4:d8:c5:8a:15:22:ee:f7:33:f1:54:4a:3e:
	         72:51:53:6f:c2:17:d2:1c:64:77:30:48:87:58:19:f6
	*/

	x509ClientCsr := `-----BEGIN CERTIFICATE REQUEST-----
MIH8MIGkAgEAMEIxFTATBgNVBAoTDHN5c3RlbTpub2RlczEpMCcGA1UEAxMgc3lz
dGVtOm5vZGU6b3N0ZXN0LWV4dHJhd29ya2VyLTMwWTATBgcqhkjOPQIBBggqhkjO
PQMBBwNCAATz0wJNo7QzR5RUIDbk4GBTRlAzcT0XLajQyckiXQjxowIIBuymBURX
QNCWGLHWCFEwAC95wDZHZQJvxGdSFL9goAAwCgYIKoZIzj0EAwIDRwAwRAIgEwZN
IL8h1uCf5/1b5lgGzzIvO2OC+4nS8JlqK8KEh4QCIFl2TcTYxYoVIu73M/FUSj5y
UVNvwhfSHGR3MEiHWBn2
-----END CERTIFICATE REQUEST-----
`
	/* Decoded output of PEM formatted server CSR

	Certificate Request:
	    Data:
	        Version: 1 (0x0)
	        Subject: O = system:nodes, CN = system:node:ostest-extraworker-3
	        Subject Public Key Info:
	            Public Key Algorithm: id-ecPublicKey
	                Public-Key: (256 bit)
	                pub:
	                    04:04:dc:cd:e4:ae:6f:5c:62:e3:bd:da:89:5e:4c:
	                    20:81:e2:16:ea:31:2b:23:5a:94:22:54:9d:d2:65:
	                    db:aa:1e:17:82:29:1a:53:84:3d:03:13:ae:ca:e3:
	                    c9:7d:13:83:b4:23:84:a3:ac:18:4b:99:38:42:43:
	                    c7:97:6d:37:0c
	                ASN1 OID: prime256v1
	                NIST CURVE: P-256
	        Attributes:
	        Requested Extensions:
	            X509v3 Subject Alternative Name:
	                DNS:ostest-extraworker-3, IP Address:192.168.111.28
	    Signature Algorithm: ecdsa-with-SHA256
	         30:46:02:21:00:c1:fa:af:ae:e3:7e:b6:d8:2d:11:ce:a7:07:
	         e6:9c:52:46:4d:34:f2:ab:ae:bd:bc:ae:49:5e:d3:91:b5:42:
	         aa:02:21:00:a8:a0:3a:01:af:5e:55:4d:5e:4b:44:62:4b:f2:
	         f3:e8:7c:11:b3:69:80:4c:d6:39:16:ba:59:3a:07:4c:dd:c2

	*/
	x509ServerCSR := `-----BEGIN CERTIFICATE REQUEST-----
MIIBNjCB3AIBADBCMRUwEwYDVQQKEwxzeXN0ZW06bm9kZXMxKTAnBgNVBAMTIHN5
c3RlbTpub2RlOm9zdGVzdC1leHRyYXdvcmtlci0zMFkwEwYHKoZIzj0CAQYIKoZI
zj0DAQcDQgAEBNzN5K5vXGLjvdqJXkwggeIW6jErI1qUIlSd0mXbqh4XgikaU4Q9
AxOuyuPJfRODtCOEo6wYS5k4QkPHl203DKA4MDYGCSqGSIb3DQEJDjEpMCcwJQYD
VR0RBB4wHIIUb3N0ZXN0LWV4dHJhd29ya2VyLTOHBMCobxwwCgYIKoZIzj0EAwID
SQAwRgIhAMH6r67jfrbYLRHOpwfmnFJGTTTyq669vK5JXtORtUKqAiEAqKA6Aa9e
VU1eS0RiS/Lz6HwRs2mATNY5FrpZOgdM3cI=
-----END CERTIFICATE REQUEST-----
`
	CommonHostname := "ostest-extraworker-3"
	var (
		c                     client.Client
		hr                    *AgentReconciler
		ctx                   = context.Background()
		mockCtrl              *gomock.Controller
		backEndCluster        *common.Cluster
		hostRequest           ctrl.Request
		agentKey              types.NamespacedName
		hostId                strfmt.UUID
		mockInstallerInternal *bminventory.MockInstallerInternals
		mockClientFactory     *MockSpokeK8sClientFactory
	)
	newAgentClusterInstall := func(name, namespace string) *hiveext.AgentClusterInstall {
		return &hiveext.AgentClusterInstall{
			Spec: hiveext.AgentClusterInstallSpec{
				Networking: hiveext.Networking{
					UserManagedNetworking: true,
				},
			},
			TypeMeta: metav1.TypeMeta{
				Kind:       "AgentClusterInstall",
				APIVersion: "hiveextension/v1beta1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
		}
	}

	generateInventory := func() string {
		inventory := &models.Inventory{
			Interfaces: []*models.Interface{
				{
					Name: "eth0",
					IPV4Addresses: []string{
						"192.168.111.28/24",
					},
					IPV6Addresses: []string{
						"1001:db8::10/120",
					},
				},
			},
			Disks: []*models.Disk{
				common.TestDefaultConfig.Disks,
			},
			Routes: common.TestDefaultRouteConfiguration,
		}

		b, err := json.Marshal(inventory)
		Expect(err).To(Not(HaveOccurred()))
		return string(b)
	}

	clientCsrs := func() *certificatesv1.CertificateSigningRequestList {
		return &certificatesv1.CertificateSigningRequestList{
			Items: []certificatesv1.CertificateSigningRequest{
				{
					Spec: certificatesv1.CertificateSigningRequestSpec{
						Request: []byte(x509ClientCsr),
						Usages: []certificatesv1.KeyUsage{
							certificatesv1.UsageDigitalSignature,
							certificatesv1.UsageKeyEncipherment,
							certificatesv1.UsageClientAuth,
						},
						Groups: []string{
							"system:serviceaccounts:openshift-machine-config-operator",
							"system:serviceaccounts",
							"system:authenticated",
						},
						Username: "system:serviceaccount:openshift-machine-config-operator:node-bootstrapper",
					},
				},
			},
		}
	}

	serverCsrs := func() *certificatesv1.CertificateSigningRequestList {
		return &certificatesv1.CertificateSigningRequestList{
			Items: []certificatesv1.CertificateSigningRequest{
				{
					Spec: certificatesv1.CertificateSigningRequestSpec{
						Request: []byte(x509ServerCSR),
						Usages: []certificatesv1.KeyUsage{
							certificatesv1.UsageDigitalSignature,
							certificatesv1.UsageKeyEncipherment,
							certificatesv1.UsageServerAuth,
						},
						Groups: []string{
							"system:authenticated",
							"system:nodes",
						},
						Username: nodeUserPrefix + CommonHostname,
					},
				},
			},
		}
	}

	approveCsrs := func(csrs *certificatesv1.CertificateSigningRequestList) *certificatesv1.CertificateSigningRequestList {
		csrs.Items[0].Status.Conditions = append(csrs.Items[0].Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
			Type:           certificatesv1.CertificateApproved,
			Reason:         "NodeCSRApprove",
			Message:        "This CSR was approved by the assisted-service",
			Status:         corev1.ConditionTrue,
			LastUpdateTime: metav1.Now(),
		})
		return csrs
	}

	approvedClientCsrs := func() *certificatesv1.CertificateSigningRequestList {
		return approveCsrs(clientCsrs())
	}

	approvedServerCsrs := func() *certificatesv1.CertificateSigningRequestList {
		return approveCsrs(serverCsrs())
	}

	BeforeEach(func() {
		c = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		mockCtrl = gomock.NewController(GinkgoT())
		mockInstallerInternal = bminventory.NewMockInstallerInternals(mockCtrl)
		mockClientFactory = NewMockSpokeK8sClientFactory(mockCtrl)
		hr = &AgentReconciler{
			Client:                     c,
			Scheme:                     scheme.Scheme,
			Log:                        common.GetTestLog(),
			Installer:                  mockInstallerInternal,
			APIReader:                  c,
			SpokeK8sClientFactory:      mockClientFactory,
			ApproveCsrsRequeueDuration: time.Minute,
		}
		sId := strfmt.UUID(uuid.New().String())
		hostId = strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:        &hostId,
				ClusterID: &sId,
				Kind:      swag.String(models.HostKindAddToExistingClusterHost),
				Inventory: generateInventory(),
				Status:    swag.String(models.HostStatusAddedToExistingCluster),
				Progress: &models.HostProgressInfo{
					CurrentStage: models.HostStageDone,
				},
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		aci := newAgentClusterInstall("test-cluster-aci", testNamespace)
		Expect(c.Create(ctx, aci)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		agentKey = types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
		secretName := fmt.Sprintf(adminKubeConfigStringTemplate, clusterDeployment.Name)
		adminKubeconfigSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: clusterDeployment.Namespace,
			},
			Data: map[string][]byte{
				"kubeconfig": []byte(BASIC_KUBECONFIG),
			},
		}
		Expect(c.Create(ctx, adminKubeconfigSecret)).ShouldNot(HaveOccurred())
	})

	tests := []struct {
		name            string
		hostname        string
		createClient    bool
		node            *corev1.Node
		nodeError       error
		csrs            *certificatesv1.CertificateSigningRequestList
		approveExpected bool
		initialStage    models.HostStage
		expectedResult  ctrl.Result
		expectedStatus  string
		expectedStage   models.HostStage
	}{
		{
			name:           "No matching node - No csrs",
			createClient:   true,
			csrs:           &certificatesv1.CertificateSigningRequestList{},
			nodeError:      &notFoundError{},
			expectedResult: ctrl.Result{RequeueAfter: time.Minute},
			expectedStatus: models.HostStatusAddedToExistingCluster,
			expectedStage:  models.HostStageRebooting,
		},
		{
			name:         "Not ready matching node - 1 csrs",
			createClient: true,
			hostname:     CommonHostname,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: CommonHostname,
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{
							Type:   corev1.NodeReady,
							Status: corev1.ConditionFalse,
						},
					},
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeInternalIP,
							Address: "192.168.111.28",
						},
					},
				},
			},
			csrs:            serverCsrs(),
			approveExpected: true,
			expectedResult: ctrl.Result{
				RequeueAfter: time.Minute,
			},
			expectedStatus: models.HostStatusAddedToExistingCluster,
			expectedStage:  models.HostStageRebooting,
		},
		{
			name:            "Get node error",
			createClient:    true,
			hostname:        CommonHostname,
			approveExpected: false,
			nodeError:       errors.New("Stam"),
			expectedResult: ctrl.Result{
				RequeueAfter: time.Minute,
			},
			expectedStatus: models.HostStatusAddedToExistingCluster,
			expectedStage:  models.HostStageRebooting,
		},
		{
			name:            "Node not found with server CSR",
			createClient:    true,
			hostname:        CommonHostname,
			csrs:            serverCsrs(),
			approveExpected: false,
			nodeError:       &notFoundError{},
			expectedResult: ctrl.Result{
				RequeueAfter: time.Minute,
			},
			expectedStatus: models.HostStatusAddedToExistingCluster,
			expectedStage:  models.HostStageRebooting,
		},
		{
			name:         "Not done Server CSR",
			createClient: true,
			hostname:     CommonHostname,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: CommonHostname,
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{},
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeInternalIP,
							Address: "192.168.111.28",
						},
					},
				},
			},
			csrs:            serverCsrs(),
			approveExpected: true,
			expectedResult: ctrl.Result{
				RequeueAfter: time.Minute,
			},
			expectedStatus: models.HostStatusAddedToExistingCluster,
			expectedStage:  models.HostStageRebooting,
		},
		{
			name:         "Done Server CSR",
			createClient: true,
			hostname:     CommonHostname,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: CommonHostname,
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{
							Type:   corev1.NodeReady,
							Status: corev1.ConditionTrue,
						},
					},
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeInternalIP,
							Address: "192.168.111.28",
						},
					},
				},
			},
			csrs:            serverCsrs(),
			approveExpected: true,
			expectedResult:  ctrl.Result{},
			expectedStatus:  models.HostStatusAddedToExistingCluster,
			expectedStage:   models.HostStageDone,
		},
		{
			name:            "Not approved client CSR",
			createClient:    true,
			hostname:        CommonHostname,
			nodeError:       &notFoundError{},
			csrs:            clientCsrs(),
			approveExpected: true,
			expectedResult: ctrl.Result{
				RequeueAfter: time.Minute,
			},
			expectedStatus: models.HostStatusAddedToExistingCluster,
			expectedStage:  models.HostStageRebooting,
		},
		{
			name:            "Approved client CSR",
			createClient:    true,
			hostname:        CommonHostname,
			nodeError:       &notFoundError{},
			csrs:            approvedClientCsrs(),
			approveExpected: false,
			expectedResult: ctrl.Result{
				RequeueAfter: time.Minute,
			},
			expectedStatus: models.HostStatusAddedToExistingCluster,
			expectedStage:  models.HostStageRebooting,
		},
		{
			name:         "Approved Server CSR",
			createClient: true,
			hostname:     CommonHostname,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: CommonHostname,
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{},
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeInternalIP,
							Address: "192.168.111.28",
						},
					},
				},
			},
			csrs:            approvedServerCsrs(),
			approveExpected: false,
			expectedResult: ctrl.Result{
				RequeueAfter: time.Minute,
			},
			expectedStatus: models.HostStatusAddedToExistingCluster,
			expectedStage:  models.HostStageRebooting,
		},
		{
			name:           "Already done",
			createClient:   false,
			initialStage:   models.HostStageDone,
			expectedResult: ctrl.Result{},
			expectedStatus: models.HostStatusAddedToExistingCluster,
			expectedStage:  models.HostStageDone,
		},
	}

	for i := range tests {
		t := &tests[i]
		It(t.name, func() {
			agentSpec := v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}}
			if t.hostname != "" {
				agentSpec.Hostname = t.hostname
			}
			host := newAgent(hostId.String(), testNamespace, agentSpec)
			host.Spec.Approved = true
			mockInstallerInternal.EXPECT().UpdateHostApprovedInternal(gomock.Any(), gomock.Any(), gomock.Any(), true).Return(nil)
			if t.initialStage != "" {
				host.Status.Progress.CurrentStage = t.initialStage
			}
			Expect(c.Create(ctx, host)).To(BeNil())
			if t.createClient {
				mockClient := NewMockSpokeK8sClient(mockCtrl)
				mockClientFactory.EXPECT().Create(gomock.Any()).Return(mockClient, nil)
				if t.node != nil || t.nodeError != nil {
					mockClient.EXPECT().GetNode(gomock.Any()).Return(t.node, t.nodeError)
				}
				if t.csrs != nil {
					mockClient.EXPECT().ListCsrs().Return(t.csrs, nil)
				}
				if t.approveExpected {
					mockClient.EXPECT().ApproveCsr(gomock.Any()).Return(nil)
				}
			}
			hostRequest = newHostRequest(host)
			result, err := hr.Reconcile(ctx, hostRequest)
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(Equal(t.expectedResult))
			agent := &v1beta1.Agent{}
			Expect(c.Get(ctx, agentKey, agent)).To(BeNil())
			Expect(agent.Status.DebugInfo.State).To(Equal(t.expectedStatus))
			Expect(agent.Status.Progress.CurrentStage).To(Equal(t.expectedStage))
		})
	}

	AfterEach(func() {
		mockCtrl.Finish()
	})
})

var _ = Describe("TestConditions", func() {
	var (
		c                     client.Client
		hr                    *AgentReconciler
		ctx                   = context.Background()
		mockCtrl              *gomock.Controller
		backEndCluster        *common.Cluster
		hostRequest           ctrl.Request
		agentKey              types.NamespacedName
		hostId                strfmt.UUID
		mockInstallerInternal *bminventory.MockInstallerInternals
	)

	BeforeEach(func() {
		c = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		mockCtrl = gomock.NewController(GinkgoT())
		mockInstallerInternal = bminventory.NewMockInstallerInternals(mockCtrl)
		hr = &AgentReconciler{
			Client:    c,
			Scheme:    scheme.Scheme,
			Log:       common.GetTestLog(),
			Installer: mockInstallerInternal,
		}
		sId := strfmt.UUID(uuid.New().String())
		hostId = strfmt.UUID(uuid.New().String())
		commonHost := &common.Host{
			Host: models.Host{
				ID:        &hostId,
				ClusterID: &sId,
				Inventory: common.GenerateTestDefaultInventory(),
			},
		}
		backEndCluster = &common.Cluster{Cluster: models.Cluster{
			ID: &sId,
			Hosts: []*models.Host{
				&commonHost.Host,
			}}}
		clusterDeployment := newClusterDeployment("clusterDeployment", testNamespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", "test-cluster-aci", "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		mockInstallerInternal.EXPECT().GetHostByKubeKey(gomock.Any()).Return(commonHost, nil).AnyTimes()
		agentKey = types.NamespacedName{
			Namespace: testNamespace,
			Name:      hostId.String(),
		}
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	tests := []struct {
		name           string
		hostStatus     string
		hostApproved   bool
		statusInfo     string
		validationInfo string
		conditions     []conditionsv1.Condition
	}{
		{
			name:           "PendingForInput",
			hostStatus:     models.HostStatusPendingForInput,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking1\",\"status\":\"failure\",\"message\":\"Host check1 is not OK\"},{\"id\":\"checking2\",\"status\":\"success\",\"message\":\"Host check2 is OK\"},{\"id\":\"checking3\",\"status\":\"failure\",\"message\":\"Host check3 is not OK\"},{\"id\":\"checking4\",\"status\":\"pending\",\"message\":\"Host check4 is pending\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentNotReadyMsg,
					Reason:  v1beta1.AgentNotReadyReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstallationNotStartedMsg,
					Reason:  v1beta1.InstallationNotStartedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsUserPendingMsg + " Host check1 is not OK,Host check3 is not OK,Host check4 is pending",
					Reason:  v1beta1.ValidationsUserPendingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.BoundMsg,
					Reason:  v1beta1.BoundReason,
					Status:  corev1.ConditionTrue,
				},
			},
		},
		{
			name:           "Insufficient",
			hostStatus:     models.HostStatusInsufficient,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking1\",\"status\":\"failure\",\"message\":\"Host check1 is not OK\"},{\"id\":\"checking2\",\"status\":\"success\",\"message\":\"Host check2 is OK\"},{\"id\":\"checking3\",\"status\":\"failure\",\"message\":\"Host check3 is not OK\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentNotReadyMsg,
					Reason:  v1beta1.AgentNotReadyReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstallationNotStartedMsg,
					Reason:  v1beta1.InstallationNotStartedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsFailingMsg + " Host check1 is not OK,Host check3 is not OK",
					Reason:  v1beta1.ValidationsFailingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.BoundMsg,
					Reason:  v1beta1.BoundReason,
					Status:  corev1.ConditionTrue,
				},
			},
		},
		{
			name:           "InsufficientUnbound",
			hostStatus:     models.HostStatusInsufficientUnbound,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking1\",\"status\":\"failure\",\"message\":\"Host check1 is not OK\"},{\"id\":\"checking2\",\"status\":\"success\",\"message\":\"Host check2 is OK\"},{\"id\":\"checking3\",\"status\":\"failure\",\"message\":\"Host check3 is not OK\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentNotReadyMsg,
					Reason:  v1beta1.AgentNotReadyReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstallationNotStartedMsg,
					Reason:  v1beta1.InstallationNotStartedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsFailingMsg + " Host check1 is not OK,Host check3 is not OK",
					Reason:  v1beta1.ValidationsFailingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.UnboundMsg,
					Reason:  v1beta1.UnboundReason,
					Status:  corev1.ConditionFalse,
				},
			},
		},
		{
			name:           "Known",
			hostStatus:     models.HostStatusKnown,
			hostApproved:   true,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentReadyMsg,
					Reason:  v1beta1.AgentReadyReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstallationNotStartedMsg,
					Reason:  v1beta1.InstallationNotStartedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsPassingMsg,
					Reason:  v1beta1.ValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.BoundMsg,
					Reason:  v1beta1.BoundReason,
					Status:  corev1.ConditionTrue,
				},
			},
		},
		{
			name:           "KnownUnbound",
			hostStatus:     models.HostStatusKnownUnbound,
			hostApproved:   true,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentReadyMsg,
					Reason:  v1beta1.AgentReadyReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstallationNotStartedMsg,
					Reason:  v1beta1.InstallationNotStartedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsPassingMsg,
					Reason:  v1beta1.ValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.UnboundMsg,
					Reason:  v1beta1.UnboundReason,
					Status:  corev1.ConditionFalse,
				},
			},
		},
		{
			name:           "Known",
			hostStatus:     models.HostStatusKnown,
			hostApproved:   false,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentIsNotApprovedMsg,
					Reason:  v1beta1.AgentIsNotApprovedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstallationNotStartedMsg,
					Reason:  v1beta1.InstallationNotStartedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsPassingMsg,
					Reason:  v1beta1.ValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.BoundMsg,
					Reason:  v1beta1.BoundReason,
					Status:  corev1.ConditionTrue,
				},
			},
		},
		{
			name:           "Installed day2",
			hostStatus:     models.HostStatusAddedToExistingCluster,
			statusInfo:     "Done",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentInstallationStoppedMsg,
					Reason:  v1beta1.AgentInstallationStoppedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstalledMsg + " Done",
					Reason:  v1beta1.InstalledReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsPassingMsg,
					Reason:  v1beta1.ValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.BoundMsg,
					Reason:  v1beta1.BoundReason,
					Status:  corev1.ConditionTrue,
				},
			},
		},
		{
			name:           "Installed",
			hostStatus:     models.HostStatusInstalled,
			statusInfo:     "Done",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentInstallationStoppedMsg,
					Reason:  v1beta1.AgentInstallationStoppedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstalledMsg + " Done",
					Reason:  v1beta1.InstalledReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsPassingMsg,
					Reason:  v1beta1.ValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.BoundMsg,
					Reason:  v1beta1.BoundReason,
					Status:  corev1.ConditionTrue,
				},
			},
		},
		{
			name:           "Installing",
			hostStatus:     models.HostStatusInstalling,
			statusInfo:     "Joined",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentAlreadyInstallingMsg,
					Reason:  v1beta1.AgentAlreadyInstallingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstallationInProgressMsg + " Joined",
					Reason:  v1beta1.InstallationInProgressReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsPassingMsg,
					Reason:  v1beta1.ValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.BoundMsg,
					Reason:  v1beta1.BoundReason,
					Status:  corev1.ConditionTrue,
				},
			},
		},
		{
			name:           "Error",
			hostStatus:     models.HostStatusError,
			statusInfo:     "Done",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentInstallationStoppedMsg,
					Reason:  v1beta1.AgentInstallationStoppedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstallationFailedMsg + " Done",
					Reason:  v1beta1.InstallationFailedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsPassingMsg,
					Reason:  v1beta1.ValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.BoundMsg,
					Reason:  v1beta1.BoundReason,
					Status:  corev1.ConditionTrue,
				},
			},
		},
		{
			name:           "Discovering",
			hostStatus:     models.HostStatusDiscovering,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentNotReadyMsg,
					Reason:  v1beta1.AgentNotReadyReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstallationNotStartedMsg,
					Reason:  v1beta1.InstallationNotStartedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsPassingMsg,
					Reason:  v1beta1.ValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.BoundMsg,
					Reason:  v1beta1.BoundReason,
					Status:  corev1.ConditionTrue,
				},
			},
		},
		{
			name:           "DiscoveringUnbound",
			hostStatus:     models.HostStatusDiscoveringUnbound,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentNotReadyMsg,
					Reason:  v1beta1.AgentNotReadyReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstallationNotStartedMsg,
					Reason:  v1beta1.InstallationNotStartedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsPassingMsg,
					Reason:  v1beta1.ValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.UnboundMsg,
					Reason:  v1beta1.UnboundReason,
					Status:  corev1.ConditionFalse,
				},
			},
		},
		{
			name:           "Disconnected",
			hostStatus:     models.HostStatusDisconnected,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentNotReadyMsg,
					Reason:  v1beta1.AgentNotReadyReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentDisonnectedMsg,
					Reason:  v1beta1.AgentDisconnectedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstallationNotStartedMsg,
					Reason:  v1beta1.InstallationNotStartedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsPassingMsg,
					Reason:  v1beta1.ValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.BoundMsg,
					Reason:  v1beta1.BoundReason,
					Status:  corev1.ConditionTrue,
				},
			},
		},
		{
			name:           "Binding",
			hostStatus:     models.HostStatusBinding,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.BindingMsg,
					Reason:  v1beta1.BindingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.BindingMsg,
					Reason:  v1beta1.BindingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.BindingMsg,
					Reason:  v1beta1.BindingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.BindingMsg,
					Reason:  v1beta1.BindingReason,
					Status:  corev1.ConditionFalse,
				},
			},
		},
		{
			name:           "Unbinding",
			hostStatus:     models.HostStatusUnbinding,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.UnbindingMsg,
					Reason:  v1beta1.UnbindingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.UnbindingMsg,
					Reason:  v1beta1.UnbindingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.UnbindingMsg,
					Reason:  v1beta1.UnbindingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.UnbindingMsg,
					Reason:  v1beta1.UnbindingReason,
					Status:  corev1.ConditionFalse,
				},
			},
		},
		{
			name:           "Unbinding Pending User Action",
			hostStatus:     models.HostStatusUnbindingPendingUserAction,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.UnbindingMsg,
					Reason:  v1beta1.UnbindingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentConnectedMsg,
					Reason:  v1beta1.AgentConnectedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.UnbindingMsg,
					Reason:  v1beta1.UnbindingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.UnbindingMsg,
					Reason:  v1beta1.UnbindingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.UnbindingPendingUserActionMsg,
					Reason:  v1beta1.UnbindingPendingUserActionReason,
					Status:  corev1.ConditionFalse,
				},
			},
		},
		{
			name:           "DisconnectedUnbound",
			hostStatus:     models.HostStatusDisconnectedUnbound,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking\",\"status\":\"success\",\"message\":\"Host is checked\"}]}",
			conditions: []conditionsv1.Condition{
				{
					Type:    v1beta1.RequirementsMetCondition,
					Message: v1beta1.AgentNotReadyMsg,
					Reason:  v1beta1.AgentNotReadyReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ConnectedCondition,
					Message: v1beta1.AgentDisonnectedMsg,
					Reason:  v1beta1.AgentDisconnectedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.InstalledCondition,
					Message: v1beta1.InstallationNotStartedMsg,
					Reason:  v1beta1.InstallationNotStartedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    v1beta1.ValidatedCondition,
					Message: v1beta1.AgentValidationsPassingMsg,
					Reason:  v1beta1.ValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    v1beta1.BoundCondition,
					Message: v1beta1.UnboundMsg,
					Reason:  v1beta1.UnboundReason,
					Status:  corev1.ConditionFalse,
				},
			},
		},
	}
	for i := range tests {
		t := tests[i]
		It(t.name, func() {
			backEndCluster.Hosts[0].Status = swag.String(t.hostStatus)
			backEndCluster.Hosts[0].StatusInfo = swag.String(t.statusInfo)
			backEndCluster.Hosts[0].ValidationsInfo = t.validationInfo

			host := newAgent(hostId.String(), testNamespace, v1beta1.AgentSpec{ClusterDeploymentName: &v1beta1.ClusterReference{Name: "clusterDeployment", Namespace: testNamespace}})
			if t.hostApproved {
				host.Spec.Approved = true
				mockInstallerInternal.EXPECT().UpdateHostApprovedInternal(gomock.Any(), gomock.Any(), gomock.Any(), true).Return(nil)
			}
			Expect(c.Create(ctx, host)).To(BeNil())

			hostRequest = newHostRequest(host)
			result, err := hr.Reconcile(ctx, hostRequest)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
			agent := &v1beta1.Agent{}
			agent.Spec.Approved = true
			Expect(c.Get(ctx, agentKey, agent)).To(BeNil())
			for _, cond := range t.conditions {
				Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, cond.Type).Message).To(Equal(cond.Message))
				Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, cond.Type).Reason).To(Equal(cond.Reason))
				Expect(conditionsv1.FindStatusCondition(agent.Status.Conditions, cond.Type).Status).To(Equal(cond.Status))
			}
			Expect(agent.Status.DebugInfo.State).To(Equal(t.hostStatus))
			Expect(agent.Status.DebugInfo.StateInfo).To(Equal(t.statusInfo))
		})
	}
})
