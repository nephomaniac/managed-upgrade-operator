package preflight

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	cvMocks "github.com/openshift/managed-upgrade-operator/pkg/clusterversion/mocks"
	configMocks "github.com/openshift/managed-upgrade-operator/pkg/configmanager/mocks"
	dvoMocks "github.com/openshift/managed-upgrade-operator/pkg/dvo/mocks"
	machineryMocks "github.com/openshift/managed-upgrade-operator/pkg/machinery/mocks"
	"github.com/openshift/managed-upgrade-operator/pkg/metrics"
	metricsMocks "github.com/openshift/managed-upgrade-operator/pkg/metrics/mocks"
	"github.com/openshift/managed-upgrade-operator/util/mocks"
	"go.uber.org/mock/gomock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openshift/managed-upgrade-operator/pkg/clusterversion"
	"github.com/openshift/managed-upgrade-operator/pkg/machinery"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// testPanicCheck is a check that always panics — used to exercise runCheck panic recovery.
type testPanicCheck struct{}

func (c *testPanicCheck) Name() string        { return "TestPanic" }
func (c *testPanicCheck) Description() string { return "always panics" }
func (c *testPanicCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerServiceTeam
}
func (c *testPanicCheck) Run(_ context.Context, _ CheckDependencies, _ logr.Logger, _ time.Duration) CheckOutput {
	panic("something went wrong")
}

var _ = Describe("PreflightController", func() {
	var (
		mockCtrl              *gomock.Controller
		mockClient            *mocks.MockClient
		mockSubResourceClient *mocks.MockSubResourceClient
		mockConfigMgrBuilder  *configMocks.MockConfigManagerBuilder
		mockConfigMgr         *configMocks.MockConfigManager
		mockCvBuilder         *cvMocks.MockClusterVersionBuilder
		mockCv                *cvMocks.MockClusterVersion
		mockMetricsBuilder    *metricsMocks.MockMetricsBuilder
		mockMetrics           *metricsMocks.MockMetrics
		mockMachinery         *machineryMocks.MockMachinery
		mockDvoClientBuilder  *dvoMocks.MockDvoClientBuilder
		mockDvoClient         *dvoMocks.MockDvoClient

		reconciler *ReconcileUpgradePreflightCheck
		req        reconcile.Request
		instance   *upgradev1alpha1.UpgradePreflightCheck
	)

	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		mockClient = mocks.NewMockClient(mockCtrl)
		mockSubResourceClient = mocks.NewMockSubResourceClient(mockCtrl)
		mockConfigMgrBuilder = configMocks.NewMockConfigManagerBuilder(mockCtrl)
		mockConfigMgr = configMocks.NewMockConfigManager(mockCtrl)
		mockCvBuilder = cvMocks.NewMockClusterVersionBuilder(mockCtrl)
		mockCv = cvMocks.NewMockClusterVersion(mockCtrl)
		mockMetricsBuilder = metricsMocks.NewMockMetricsBuilder(mockCtrl)
		mockMetrics = metricsMocks.NewMockMetrics(mockCtrl)
		mockMachinery = machineryMocks.NewMockMachinery(mockCtrl)
		mockDvoClientBuilder = dvoMocks.NewMockDvoClientBuilder(mockCtrl)
		mockDvoClient = dvoMocks.NewMockDvoClient(mockCtrl)

		reconciler = &ReconcileUpgradePreflightCheck{
			Client:               mockClient,
			MetricsClientBuilder: mockMetricsBuilder,
			CvClientBuilder:      mockCvBuilder,
			Machinery:            mockMachinery,
			ConfigManagerBuilder: mockConfigMgrBuilder,
			DvoClientBuilder:     mockDvoClientBuilder,
		}

		req = reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-preflight",
				Namespace: "openshift-managed-upgrade-operator",
			},
		}

		instance = &upgradev1alpha1.UpgradePreflightCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-preflight",
				Namespace: "openshift-managed-upgrade-operator",
			},
			Spec: upgradev1alpha1.UpgradePreflightCheckSpec{
				UpgradeConfigSpec: upgradev1alpha1.UpgradeConfigSpec{
					Type: "OSD",
					Desired: upgradev1alpha1.Update{
						Version: "4.16.15",
						Channel: "fast-4.16",
					},
				},
			},
		}
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	// gateConfig sets up ConfigManagerBuilder mock to return the given enabled feature gates.
	enableGate := func() {
		mockConfigMgrBuilder.EXPECT().New(gomock.Any(), gomock.Any()).Return(mockConfigMgr)
		mockConfigMgr.EXPECT().Into(gomock.Any()).DoAndReturn(func(cfg interface{}) error {
			c, ok := cfg.(*preflightGateConfig)
			if !ok {
				return fmt.Errorf("expected *preflightGateConfig, got %T", cfg)
			}
			c.FeatureGate.Enabled = []string{preflightControllerFeatureGate}
			return nil
		})
	}

	disableGate := func() {
		mockConfigMgrBuilder.EXPECT().New(gomock.Any(), gomock.Any()).Return(mockConfigMgr)
		mockConfigMgr.EXPECT().Into(gomock.Any()).Return(nil) // enabled list stays empty
	}

	Describe("Reconcile", func() {
		Context("when the feature gate is not enabled", func() {
			It("returns without error and does nothing", func() {
				disableGate()
				result, err := reconciler.Reconcile(context.TODO(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}))
			})
		})

		Context("when the ConfigMap cannot be read", func() {
			It("returns without error (inactive)", func() {
				mockConfigMgrBuilder.EXPECT().New(gomock.Any(), gomock.Any()).Return(mockConfigMgr)
				mockConfigMgr.EXPECT().Into(gomock.Any()).Return(fmt.Errorf("configmap not found"))
				result, err := reconciler.Reconcile(context.TODO(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}))
			})
		})

		Context("when the UpgradePreflightCheck CR is not found", func() {
			It("returns without error", func() {
				enableGate()
				mockClient.EXPECT().Get(gomock.Any(), req.NamespacedName, gomock.Any()).
					Return(apierrors.NewNotFound(schema.GroupResource{}, "test-preflight"))
				result, err := reconciler.Reconcile(context.TODO(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}))
			})
		})

		Context("when the CR is already completed", func() {
			It("returns without re-running checks", func() {
				enableGate()
				now := metav1.Now()
				instance.Status.CompletedAt = &now
				mockClient.EXPECT().Get(gomock.Any(), req.NamespacedName, gomock.Any()).
					DoAndReturn(func(_ context.Context, _ types.NamespacedName, obj interface{}, _ ...interface{}) error {
						typedObj, ok := obj.(*upgradev1alpha1.UpgradePreflightCheck)
						if !ok {
							return fmt.Errorf("expected *UpgradePreflightCheck, got %T", obj)
						}
						*typedObj = *instance
						return nil
					})
				result, err := reconciler.Reconcile(context.TODO(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}))
			})
		})

		Context("when the CR limit is exceeded", func() {
			It("writes rejection status and returns without running checks", func() {
				enableGate()
				mockClient.EXPECT().Get(gomock.Any(), req.NamespacedName, gomock.Any()).
					DoAndReturn(func(_ context.Context, _ types.NamespacedName, obj interface{}, _ ...interface{}) error {
						typedObj := obj.(*upgradev1alpha1.UpgradePreflightCheck)
						*typedObj = *instance
						return nil
					})
				// pruneAndCount returns 10 existing CRs (at limit)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&upgradev1alpha1.UpgradePreflightCheckList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						l := list.(*upgradev1alpha1.UpgradePreflightCheckList)
						fresh := metav1.NewTime(time.Now())
						for i := 0; i < maxPreflightChecks; i++ {
							l.Items = append(l.Items, upgradev1alpha1.UpgradePreflightCheck{
								ObjectMeta: metav1.ObjectMeta{
									Name:              fmt.Sprintf("existing-%d", i),
									CreationTimestamp: fresh,
								},
							})
						}
						return nil
					})
				// rejection status update
				mockClient.EXPECT().Status().Return(mockSubResourceClient)
				mockSubResourceClient.EXPECT().Update(gomock.Any(), gomock.Any()).Return(nil)

				result, err := reconciler.Reconcile(context.TODO(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}))
			})
		})

		Context("when the CR has not started yet", func() {
			It("sets StartedAt then runs checks", func() {
				enableGate()
				mockClient.EXPECT().Get(gomock.Any(), req.NamespacedName, gomock.Any()).
					DoAndReturn(func(_ context.Context, _ types.NamespacedName, obj interface{}, _ ...interface{}) error {
						typedObj, ok := obj.(*upgradev1alpha1.UpgradePreflightCheck)
						if !ok {
							return fmt.Errorf("expected *UpgradePreflightCheck, got %T", obj)
						}
						*typedObj = *instance
						return nil
					})
				// pruneAndCount: list all CRs (empty → 0, well below limit)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&upgradev1alpha1.UpgradePreflightCheckList{}), gomock.Any()).
					Return(nil)

				// StartedAt update
				mockClient.EXPECT().Status().Return(mockSubResourceClient)
				mockSubResourceClient.EXPECT().Update(gomock.Any(), gomock.Any()).Return(nil)

				// check calls (all checks enabled)
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv).AnyTimes()
				mockCv.EXPECT().GetClusterVersion().Return(&configv1.ClusterVersion{}, nil).Times(3) // ClusterVersion + UpgradeInFlight + UpgradeableCondition
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&configv1.ClusterOperatorList{}), gomock.Any()).Return(nil) // extended CO operator list
				mockCv.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{}, nil).Times(2) // baseline CO + extended CO
				// 6 NewClient calls: baseline CO, baseline CA, extended CA, baseline MCN, baseline NT, baseline PDB
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(mockMetrics, nil).Times(6)
				mockMetrics.EXPECT().Query(gomock.Any()).Return(&metrics.AlertResponse{}, nil).Times(2) // extended CA + baseline CA
				// metric update calls from upgraders functions (baseline CO, CA, MCN, NT, PDB on success)
				mockMetrics.EXPECT().UpdateMetricHealthcheckSucceeded(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				mockMetrics.EXPECT().UpdateMetricHealthcheckFailed(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				// node list: 2 extended (MCN + NT) + 2 baseline (MCN + NT via upgraders) + 1 PDB master/infra
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					Return(nil).Times(5)
				// pod list: 1 for PDB master/infra extended check
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.PodList{}), gomock.Any()).
					Return(nil)
				// daemonset list: 1 for PDB master/infra extended check
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&appsv1.DaemonSetList{}), gomock.Any()).
					Return(nil)
				// PDB list: 1 master/infra extended + 1 baseline (via upgraders.HealthCheckPDB)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&policyv1.PodDisruptionBudgetList{}), gomock.Any()).
					Return(nil).Times(2)
				// baseline PDB DVO check
				mockDvoClientBuilder.EXPECT().New(gomock.Any()).Return(mockDvoClient, nil)
				mockDvoClient.EXPECT().GetMetrics().Return([]byte(""), nil)
				// PullSecretValid: GetAccessToken reads ClusterVersion + pull-secret Secret
				mockClient.EXPECT().Get(gomock.Any(), types.NamespacedName{Name: "version"}, gomock.AssignableToTypeOf(&configv1.ClusterVersion{})).
					Return(nil)
				mockClient.EXPECT().Get(gomock.Any(), types.NamespacedName{Namespace: "openshift-config", Name: "pull-secret"}, gomock.AssignableToTypeOf(&corev1.Secret{})).
					DoAndReturn(func(_ context.Context, _ types.NamespacedName, obj interface{}, _ ...interface{}) error {
						s := obj.(*corev1.Secret)
						s.Data = map[string][]byte{
							".dockerconfigjson": []byte(`{"auths":{"cloud.openshift.com":{"auth":"dGVzdA=="}}}`),
						}
						return nil
					})
				// AdminAcks: reads admin-acks ConfigMap
				mockClient.EXPECT().Get(gomock.Any(), types.NamespacedName{Namespace: "openshift-config", Name: "admin-acks"}, gomock.AssignableToTypeOf(&corev1.ConfigMap{})).
					Return(nil)

				// CompletedAt update
				mockClient.EXPECT().Status().Return(mockSubResourceClient)
				mockSubResourceClient.EXPECT().Update(gomock.Any(), gomock.Any()).Return(nil)

				result, err := reconciler.Reconcile(context.TODO(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}))
			})
		})
	})

	Describe("pruneAndCount", func() {
		It("returns 0 when no CRs exist", func() {
			mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&upgradev1alpha1.UpgradePreflightCheckList{}), gomock.Any()).
				Return(nil)
			n, err := reconciler.pruneAndCount(context.TODO(), "openshift-managed-upgrade-operator", "", log)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(0))
		})

		It("returns an error when List fails", func() {
			mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&upgradev1alpha1.UpgradePreflightCheckList{}), gomock.Any()).
				Return(fmt.Errorf("list failed"))
			_, err := reconciler.pruneAndCount(context.TODO(), "openshift-managed-upgrade-operator", "", log)
			Expect(err).To(HaveOccurred())
		})

		It("deletes expired CRs and excludes them from the count", func() {
			old := metav1.NewTime(time.Now().Add(-25 * time.Hour))
			expiredCR := upgradev1alpha1.UpgradePreflightCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "expired-check",
					Namespace:         "openshift-managed-upgrade-operator",
					CreationTimestamp: old,
				},
			}
			mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&upgradev1alpha1.UpgradePreflightCheckList{}), gomock.Any()).
				DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
					l := list.(*upgradev1alpha1.UpgradePreflightCheckList)
					l.Items = []upgradev1alpha1.UpgradePreflightCheck{expiredCR}
					return nil
				})
			mockClient.EXPECT().Delete(gomock.Any(), gomock.Any()).Return(nil)
			n, err := reconciler.pruneAndCount(context.TODO(), "openshift-managed-upgrade-operator", "", log)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(0))
		})

		It("skips the named CR and counts it", func() {
			fresh := metav1.NewTime(time.Now())
			current := upgradev1alpha1.UpgradePreflightCheck{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "current",
					Namespace:         "openshift-managed-upgrade-operator",
					CreationTimestamp: fresh,
				},
			}
			mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&upgradev1alpha1.UpgradePreflightCheckList{}), gomock.Any()).
				DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
					l := list.(*upgradev1alpha1.UpgradePreflightCheckList)
					l.Items = []upgradev1alpha1.UpgradePreflightCheck{current}
					return nil
				})
			n, err := reconciler.pruneAndCount(context.TODO(), "openshift-managed-upgrade-operator", "current", log)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(1))
		})
	})

	Describe("checkAllowed", func() {
		It("allows all checks when the allowlist is empty", func() {
			Expect(checkAllowed("CriticalAlertsExtended", nil)).To(BeTrue())
			Expect(checkAllowed("ClusterOperatorsExtended", []string{})).To(BeTrue())
		})

		It("allows only listed checks when allowlist is non-empty", func() {
			list := []string{"CriticalAlertsExtended", "ClusterOperatorsExtended"}
			Expect(checkAllowed("CriticalAlertsExtended", list)).To(BeTrue())
			Expect(checkAllowed("ClusterOperatorsExtended", list)).To(BeTrue())
			Expect(checkAllowed("PodDisruptionBudgetsMasterInfraExtended", list)).To(BeFalse())
		})
	})

	Describe("runCheck framework", func() {
		It("returns a completed PreflightCheckResult on successful check execution", func() {
			check := &clusterVersionCheck{}
			deps := CheckDependencies{Client: mockClient, CvClientBuilder: mockCvBuilder}
			mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
			mockCv.EXPECT().GetClusterVersion().Return(&configv1.ClusterVersion{
				Status: configv1.ClusterVersionStatus{Desired: configv1.Release{Version: "4.16.10"}},
			}, nil)
			result := runCheck(check, context.TODO(), deps, log, defaultCheckTimeout)
			Expect(result.Execution.State).To(Equal(upgradev1alpha1.CheckCompleted))
			Expect(result.Result).To(Equal(upgradev1alpha1.CheckPassed))
			Expect(result.Description).NotTo(BeEmpty())
			Expect(result.Execution.StartedAt).NotTo(BeNil())
			Expect(result.Execution.CompletedAt).NotTo(BeNil())
		})

		It("recovers from a panic and returns State=Error with service-team owner", func() {
			check := &testPanicCheck{}
			deps := CheckDependencies{}
			result := runCheck(check, context.TODO(), deps, log, defaultCheckTimeout)
			Expect(result.Execution.State).To(Equal(upgradev1alpha1.CheckError))
			Expect(result.Execution.Error).To(ContainSubstring("something went wrong"))
			Expect(result.Owner).To(Equal(upgradev1alpha1.OwnerServiceTeam))
			Expect(result.Action).NotTo(BeEmpty())
		})
	})

	Describe("checkTimeout", func() {
		It("returns the default when CheckTimeoutSeconds is 0", func() {
			Expect(checkTimeout(instance)).To(Equal(defaultCheckTimeout))
		})

		It("returns the spec value when set", func() {
			instance.Spec.CheckTimeoutSeconds = 60
			Expect(checkTimeout(instance)).To(Equal(60 * time.Second))
		})
	})

	Describe("pdbViolation", func() {
		It("returns empty string for a benign PDB", func() {
			pdb := policyv1.PodDisruptionBudget{}
			Expect(pdbViolation(pdb)).To(BeEmpty())
		})

		It("detects MaxUnavailable=0 (int)", func() {
			zero := intstr.FromInt(0)
			pdb := policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &zero},
			}
			Expect(pdbViolation(pdb)).To(Equal("MaxUnavailable=0"))
		})

		It("allows MaxUnavailable=1 (int)", func() {
			one := intstr.FromInt(1)
			pdb := policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &one},
			}
			Expect(pdbViolation(pdb)).To(BeEmpty())
		})

		It("detects MaxUnavailable=0% (string)", func() {
			zero := intstr.FromString("0%")
			pdb := policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &zero},
			}
			Expect(pdbViolation(pdb)).To(Equal("MaxUnavailable=0%"))
		})

		It("allows MaxUnavailable=10% (string)", func() {
			pct := intstr.FromString("10%")
			pdb := policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &pct},
			}
			Expect(pdbViolation(pdb)).To(BeEmpty())
		})

		It("detects MinAvailable=100% (string)", func() {
			full := intstr.FromString("100%")
			pdb := policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &full},
			}
			Expect(pdbViolation(pdb)).To(Equal("MinAvailable=100%"))
		})

		It("allows MinAvailable=50% (string)", func() {
			half := intstr.FromString("50%")
			pdb := policyv1.PodDisruptionBudget{
				Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &half},
			}
			Expect(pdbViolation(pdb)).To(BeEmpty())
		})
	})

	Describe("individual check functions", func() {
		Context("clusterVersionCheck", func() {
			var check *clusterVersionCheck
			BeforeEach(func() { check = &clusterVersionCheck{} })

			It("returns Passed with the current version on success", func() {
				deps := CheckDependencies{Client: mockClient, CvClientBuilder: mockCvBuilder}
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
				mockCv.EXPECT().GetClusterVersion().Return(&configv1.ClusterVersion{
					Status: configv1.ClusterVersionStatus{
						Desired: configv1.Release{Version: "4.16.10"},
					},
				}, nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
				Expect(output.Findings).To(ContainElement("currentVersion=4.16.10"))
			})

			It("returns Error when GetClusterVersion fails", func() {
				deps := CheckDependencies{Client: mockClient, CvClientBuilder: mockCvBuilder}
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
				mockCv.EXPECT().GetClusterVersion().Return(nil, fmt.Errorf("cvo unavailable"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
				Expect(output.Error).To(ContainSubstring("cvo unavailable"))
			})
		})

		Context("upgradeInFlightCheck", func() {
			var check *upgradeInFlightCheck
			BeforeEach(func() { check = &upgradeInFlightCheck{} })

			It("returns Passed when no upgrade is in progress", func() {
				deps := CheckDependencies{Client: mockClient, CvClientBuilder: mockCvBuilder}
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
				mockCv.EXPECT().GetClusterVersion().Return(&configv1.ClusterVersion{
					Status: configv1.ClusterVersionStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{Type: configv1.OperatorProgressing, Status: configv1.ConditionFalse, Message: "Cluster version is 4.16.10"},
						},
					},
				}, nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Failed when an upgrade is in progress", func() {
				deps := CheckDependencies{Client: mockClient, CvClientBuilder: mockCvBuilder}
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
				mockCv.EXPECT().GetClusterVersion().Return(&configv1.ClusterVersion{
					Status: configv1.ClusterVersionStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{Type: configv1.OperatorProgressing, Status: configv1.ConditionTrue, Message: "Working towards 4.17.0"},
						},
					},
				}, nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
				Expect(output.Summary).To(ContainSubstring("4.17.0"))
				Expect(output.Owner).To(Equal(upgradev1alpha1.OwnerServiceTeam))
				Expect(output.Action).NotTo(BeEmpty())
			})

			It("returns Error when GetClusterVersion fails", func() {
				deps := CheckDependencies{Client: mockClient, CvClientBuilder: mockCvBuilder}
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
				mockCv.EXPECT().GetClusterVersion().Return(nil, fmt.Errorf("cvo unavailable"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
				Expect(output.Error).To(ContainSubstring("cvo unavailable"))
			})
		})

		Context("criticalAlertsCheck (baseline)", func() {
			var check *criticalAlertsCheck
			BeforeEach(func() { check = &criticalAlertsCheck{} })

			It("returns Passed when no critical alerts are firing", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(mockMetrics, nil)
				mockMetrics.EXPECT().Query(gomock.Any()).Return(&metrics.AlertResponse{
					Data: metrics.AlertData{Result: []metrics.AlertResult{}},
				}, nil)
				mockMetrics.EXPECT().UpdateMetricHealthcheckSucceeded(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Failed when critical alerts are firing", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(mockMetrics, nil)
				mockMetrics.EXPECT().Query(gomock.Any()).Return(&metrics.AlertResponse{
					Data: metrics.AlertData{Result: []metrics.AlertResult{
						{Metric: map[string]string{"alertname": "KubeNodeNotReady"}},
					}},
				}, nil)
				mockMetrics.EXPECT().UpdateMetricHealthcheckFailed(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
			})

			It("returns Error when the metrics client cannot be built", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(nil, fmt.Errorf("prometheus unreachable"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
			})
		})

		Context("criticalAlertsExtendedCheck", func() {
			var check *criticalAlertsExtendedCheck
			BeforeEach(func() { check = &criticalAlertsExtendedCheck{} })

			It("returns Passed when no critical alerts are firing", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(mockMetrics, nil)
				mockMetrics.EXPECT().Query(gomock.Any()).Return(&metrics.AlertResponse{
					Data: metrics.AlertData{Result: []metrics.AlertResult{}},
				}, nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Failed with alert names when critical alerts are firing", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(mockMetrics, nil)
				mockMetrics.EXPECT().Query(gomock.Any()).Return(&metrics.AlertResponse{
					Data: metrics.AlertData{Result: []metrics.AlertResult{
						{Metric: map[string]string{"alertname": "KubeNodeNotReady"}},
						{Metric: map[string]string{"alertname": "KubeNodeNotReady"}}, // duplicate
						{Metric: map[string]string{"alertname": "ClusterOperatorDown"}},
					}},
				}, nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
				Expect(output.Findings).To(ConsistOf("KubeNodeNotReady", "ClusterOperatorDown"))
			})

			It("returns Error when the metrics client cannot be built after retries", func() {
				prometheusRetryBase = 1 * time.Millisecond
				defer func() { prometheusRetryBase = 2 * time.Second }()
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).
					Return(nil, fmt.Errorf("prometheus unreachable")).
					Times(prometheusRetries)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
				Expect(output.Error).To(ContainSubstring("prometheus unreachable"))
			})

			It("returns Error when Query fails after retries", func() {
				prometheusRetryBase = 1 * time.Millisecond
				defer func() { prometheusRetryBase = 2 * time.Second }()
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(mockMetrics, nil).Times(prometheusRetries)
				mockMetrics.EXPECT().Query(gomock.Any()).Return(nil, fmt.Errorf("query error")).Times(prometheusRetries)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
				Expect(output.Error).To(ContainSubstring("query error"))
			})
		})

		Context("clusterOperatorsCheck (baseline)", func() {
			var check *clusterOperatorsCheck
			BeforeEach(func() { check = &clusterOperatorsCheck{} })

			It("returns Passed when no operators are degraded", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder, CvClientBuilder: mockCvBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(mockMetrics, nil)
				mockMetrics.EXPECT().UpdateMetricHealthcheckSucceeded(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
				mockCv.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{
					Degraded: []string{},
				}, nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Failed when operators are degraded", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder, CvClientBuilder: mockCvBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(mockMetrics, nil)
				mockMetrics.EXPECT().UpdateMetricHealthcheckFailed(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
				mockCv.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{
					Degraded: []string{"kube-apiserver"},
				}, nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
			})

			It("returns Error when metrics client cannot be built", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder, CvClientBuilder: mockCvBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(nil, fmt.Errorf("metrics unavailable"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
			})
		})

		Context("clusterOperatorsExtendedCheck", func() {
			var check *clusterOperatorsExtendedCheck
			BeforeEach(func() { check = &clusterOperatorsExtendedCheck{} })

			It("returns Passed when no operators are degraded", func() {
				deps := CheckDependencies{Client: mockClient, CvClientBuilder: mockCvBuilder}
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&configv1.ClusterOperatorList{}), gomock.Any()).Return(nil)
				mockCv.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{
					Degraded: []string{},
				}, nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Failed with degraded operator names", func() {
				deps := CheckDependencies{Client: mockClient, CvClientBuilder: mockCvBuilder}
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&configv1.ClusterOperatorList{}), gomock.Any()).Return(nil)
				mockCv.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{
					Degraded: []string{"kube-apiserver", "etcd"},
				}, nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
				Expect(output.Findings).To(ConsistOf("kube-apiserver", "etcd"))
			})

			It("returns Error when HasDegradedOperators fails", func() {
				deps := CheckDependencies{Client: mockClient, CvClientBuilder: mockCvBuilder}
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&configv1.ClusterOperatorList{}), gomock.Any()).Return(nil)
				mockCv.EXPECT().HasDegradedOperators().Return(nil, fmt.Errorf("co list failed"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
				Expect(output.Error).To(ContainSubstring("co list failed"))
			})
		})

		Context("manuallyCordonedNodesCheck (baseline)", func() {
			var check *manuallyCordonedNodesCheck
			BeforeEach(func() { check = &manuallyCordonedNodesCheck{} })

			It("returns Passed when no worker nodes are cordoned", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder, Machinery: mockMachinery}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(mockMetrics, nil)
				mockMetrics.EXPECT().UpdateMetricHealthcheckSucceeded(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					Return(nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Error when metrics client cannot be built", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder, Machinery: mockMachinery}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(nil, fmt.Errorf("metrics unavailable"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
			})
		})

		Context("manuallyCordonedNodesExtendedCheck", func() {
			var check *manuallyCordonedNodesExtendedCheck
			BeforeEach(func() { check = &manuallyCordonedNodesExtendedCheck{} })

			workerNode := func(name string) corev1.Node {
				return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
			}

			It("returns Passed when no worker nodes are cordoned", func() {
				deps := CheckDependencies{Client: mockClient, Machinery: mockMachinery}
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						nl, ok := list.(*corev1.NodeList)
						if !ok {
							return fmt.Errorf("expected *corev1.NodeList, got %T", list)
						}
						nl.Items = []corev1.Node{workerNode("worker-1")}
						return nil
					})
				mockMachinery.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false})
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Failed when a worker node is manually cordoned", func() {
				deps := CheckDependencies{Client: mockClient, Machinery: mockMachinery}
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						nl, ok := list.(*corev1.NodeList)
						if !ok {
							return fmt.Errorf("expected *corev1.NodeList, got %T", list)
						}
						nl.Items = []corev1.Node{workerNode("worker-1"), workerNode("worker-2")}
						return nil
					})
				// worker-1: cordoned by upgrader (skip), worker-2: cordoned manually (flag)
				mockMachinery.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: true}).Times(2)
				mockMachinery.EXPECT().IsNodeUpgrading(gomock.Any()).Return(true)  // worker-1: upgrading, skip
				mockMachinery.EXPECT().IsNodeUpgrading(gomock.Any()).Return(false) // worker-2: manual
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
				Expect(output.Findings).To(ContainElement(ContainSubstring("worker-2")))
			})

			It("returns Error when node list fails", func() {
				deps := CheckDependencies{Client: mockClient, Machinery: mockMachinery}
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					Return(fmt.Errorf("list nodes failed"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
				Expect(output.Error).To(ContainSubstring("list nodes failed"))
			})
		})

		Context("nodeTaintsCheck (baseline)", func() {
			var check *nodeTaintsCheck
			BeforeEach(func() { check = &nodeTaintsCheck{} })

			It("returns Passed when no nodes have pressure taints", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder, Machinery: mockMachinery}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(mockMetrics, nil)
				mockMetrics.EXPECT().UpdateMetricHealthcheckSucceeded(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					Return(nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Error when metrics client cannot be built", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder, Machinery: mockMachinery}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(nil, fmt.Errorf("metrics unavailable"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
			})
		})

		Context("nodeTaintsExtendedCheck", func() {
			var check *nodeTaintsExtendedCheck
			BeforeEach(func() { check = &nodeTaintsExtendedCheck{} })

			It("returns Passed when no nodes have pressure taints", func() {
				deps := CheckDependencies{Client: mockClient, Machinery: mockMachinery}
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						nl, ok := list.(*corev1.NodeList)
						if !ok {
							return fmt.Errorf("expected *corev1.NodeList, got %T", list)
						}
						nl.Items = []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}}
						return nil
					})
				mockMachinery.EXPECT().HasMemoryPressure(gomock.Any()).Return(false)
				mockMachinery.EXPECT().HasDiskPressure(gomock.Any()).Return(false)
				mockMachinery.EXPECT().HasPidPressure(gomock.Any()).Return(false)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Failed when a node has memory pressure", func() {
				deps := CheckDependencies{Client: mockClient, Machinery: mockMachinery}
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						nl, ok := list.(*corev1.NodeList)
						if !ok {
							return fmt.Errorf("expected *corev1.NodeList, got %T", list)
						}
						nl.Items = []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}}
						return nil
					})
				mockMachinery.EXPECT().HasMemoryPressure(gomock.Any()).Return(true)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
				Expect(output.Findings).To(ContainElement(ContainSubstring("worker-1")))
			})

			It("returns Error when node list fails", func() {
				deps := CheckDependencies{Client: mockClient, Machinery: mockMachinery}
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					Return(fmt.Errorf("list nodes failed"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
			})
		})

		Context("podDisruptionBudgetsCheck", func() {
			var check *podDisruptionBudgetsCheck
			BeforeEach(func() { check = &podDisruptionBudgetsCheck{} })

			It("returns Error when DvoClientBuilder is nil", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder, DvoClientBuilder: nil}
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
				Expect(output.Error).To(ContainSubstring("DvoClientBuilder is not configured"))
			})

			It("returns Passed when no blocking PDBs and DVO metrics are clean", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder, DvoClientBuilder: mockDvoClientBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(mockMetrics, nil)
				mockMetrics.EXPECT().UpdateMetricHealthcheckSucceeded(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&policyv1.PodDisruptionBudgetList{}), gomock.Any()).
					Return(nil)
				mockDvoClientBuilder.EXPECT().New(gomock.Any()).Return(mockDvoClient, nil)
				mockDvoClient.EXPECT().GetMetrics().Return([]byte(""), nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Error when metrics client cannot be built", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder, DvoClientBuilder: mockDvoClientBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(nil, fmt.Errorf("metrics unavailable"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
			})

			It("returns Error when PDB list fails", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder, DvoClientBuilder: mockDvoClientBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(mockMetrics, nil)
				mockMetrics.EXPECT().UpdateMetricHealthcheckFailed(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&policyv1.PodDisruptionBudgetList{}), gomock.Any()).
					Return(fmt.Errorf("pdb list failed"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
			})

			It("returns Error when DVO client cannot be created", func() {
				deps := CheckDependencies{Client: mockClient, MetricsClientBuilder: mockMetricsBuilder, DvoClientBuilder: mockDvoClientBuilder}
				mockMetricsBuilder.EXPECT().NewClient(gomock.Any()).Return(mockMetrics, nil)
				mockMetrics.EXPECT().UpdateMetricHealthcheckFailed(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&policyv1.PodDisruptionBudgetList{}), gomock.Any()).
					Return(nil)
				mockDvoClientBuilder.EXPECT().New(gomock.Any()).Return(nil, fmt.Errorf("dvo unavailable"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
			})
		})

		Context("pdbMasterInfraExtendedCheck", func() {
			var check *pdbMasterInfraExtendedCheck
			BeforeEach(func() { check = &pdbMasterInfraExtendedCheck{} })

			masterNode := func(name string) corev1.Node {
				return corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:   name,
						Labels: map[string]string{"node-role.kubernetes.io/master": ""},
					},
				}
			}
			infraNode := func(name string) corev1.Node {
				return corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:   name,
						Labels: map[string]string{"node-role.kubernetes.io/infra": ""},
					},
				}
			}
			workerNode := func(name string) corev1.Node {
				return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
			}
			podOnNode := func(namespace, name, nodeName string, labels map[string]string) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
					Spec:       corev1.PodSpec{NodeName: nodeName},
				}
			}
			masterToleration := corev1.Toleration{Key: "node-role.kubernetes.io/master", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}
			infraToleration := corev1.Toleration{Key: "node-role.kubernetes.io/infra", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}
			wildcardToleration := corev1.Toleration{Key: "", Operator: corev1.TolerationOpExists}
			dsWithTolerations := func(namespace, name string, labels map[string]string, tolerations []corev1.Toleration) appsv1.DaemonSet {
				return appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
					Spec: appsv1.DaemonSetSpec{
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: labels},
							Spec:       corev1.PodSpec{Tolerations: tolerations},
						},
					},
				}
			}

			It("returns Passed when no master/infra nodes exist", func() {
				deps := CheckDependencies{Client: mockClient}
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						nl := list.(*corev1.NodeList)
						nl.Items = []corev1.Node{workerNode("worker-1")}
						return nil
					})
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.PodList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&appsv1.DaemonSetList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&policyv1.PodDisruptionBudgetList{}), gomock.Any()).Return(nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Passed when no PDBs cover master/infra pods", func() {
				deps := CheckDependencies{Client: mockClient}
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						nl := list.(*corev1.NodeList)
						nl.Items = []corev1.Node{masterNode("master-1")}
						return nil
					})
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.PodList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						pl := list.(*corev1.PodList)
						pl.Items = []corev1.Pod{podOnNode("openshift-etcd", "etcd-master-1", "master-1", map[string]string{"app": "etcd"})}
						return nil
					})
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&appsv1.DaemonSetList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&policyv1.PodDisruptionBudgetList{}), gomock.Any()).Return(nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Failed when a restrictive PDB covers running pods on master nodes", func() {
				deps := CheckDependencies{Client: mockClient}
				zero := intstr.FromInt(0)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						nl := list.(*corev1.NodeList)
						nl.Items = []corev1.Node{masterNode("master-1")}
						return nil
					})
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.PodList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						pl := list.(*corev1.PodList)
						pl.Items = []corev1.Pod{podOnNode("customer-ns", "app-pod", "master-1", map[string]string{"app": "myapp"})}
						return nil
					})
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&appsv1.DaemonSetList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&policyv1.PodDisruptionBudgetList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						pl := list.(*policyv1.PodDisruptionBudgetList)
						pl.Items = []policyv1.PodDisruptionBudget{
							{
								ObjectMeta: metav1.ObjectMeta{Namespace: "customer-ns", Name: "myapp-pdb"},
								Spec: policyv1.PodDisruptionBudgetSpec{
									MaxUnavailable: &zero,
									Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "myapp"}},
								},
							},
						}
						return nil
					})
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
				Expect(output.Findings).To(ContainElement(ContainSubstring("myapp-pdb")))
			})

			It("returns Passed when PDB covers infra pods but is not restrictive", func() {
				deps := CheckDependencies{Client: mockClient}
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						nl := list.(*corev1.NodeList)
						nl.Items = []corev1.Node{infraNode("infra-1")}
						return nil
					})
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.PodList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						pl := list.(*corev1.PodList)
						pl.Items = []corev1.Pod{podOnNode("openshift-logging", "logging-pod", "infra-1", map[string]string{"app": "logging"})}
						return nil
					})
				one := intstr.FromInt(1)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&appsv1.DaemonSetList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&policyv1.PodDisruptionBudgetList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						pl := list.(*policyv1.PodDisruptionBudgetList)
						pl.Items = []policyv1.PodDisruptionBudget{
							{
								ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-logging", Name: "logging-pdb"},
								Spec: policyv1.PodDisruptionBudgetSpec{
									MaxUnavailable: &one,
									Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "logging"}},
								},
							},
						}
						return nil
					})
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
				Expect(output.Findings).To(ContainElement(ContainSubstring("logging-pdb")))
			})

			It("returns Failed when a restrictive PDB covers a DaemonSet targeting master nodes", func() {
				deps := CheckDependencies{Client: mockClient}
				zero := intstr.FromInt(0)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						nl := list.(*corev1.NodeList)
						nl.Items = []corev1.Node{masterNode("master-1")}
						return nil
					})
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.PodList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&appsv1.DaemonSetList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						dl := list.(*appsv1.DaemonSetList)
						dl.Items = []appsv1.DaemonSet{
							dsWithTolerations("openshift-monitoring", "node-exporter", map[string]string{"app": "node-exporter"}, []corev1.Toleration{masterToleration}),
						}
						return nil
					})
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&policyv1.PodDisruptionBudgetList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						pl := list.(*policyv1.PodDisruptionBudgetList)
						pl.Items = []policyv1.PodDisruptionBudget{
							{
								ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-monitoring", Name: "node-exporter-pdb"},
								Spec: policyv1.PodDisruptionBudgetSpec{
									MaxUnavailable: &zero,
									Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "node-exporter"}},
								},
							},
						}
						return nil
					})
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
				Expect(output.Findings).To(ContainElement(ContainSubstring("node-exporter-pdb")))
				Expect(output.Findings).To(ContainElement(ContainSubstring("node-exporter")))
			})

			It("returns Failed when a restrictive PDB covers a DaemonSet with infra toleration", func() {
				deps := CheckDependencies{Client: mockClient}
				zero := intstr.FromInt(0)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.PodList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&appsv1.DaemonSetList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						dl := list.(*appsv1.DaemonSetList)
						dl.Items = []appsv1.DaemonSet{
							dsWithTolerations("openshift-logging", "fluentd", map[string]string{"app": "fluentd"}, []corev1.Toleration{infraToleration}),
						}
						return nil
					})
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&policyv1.PodDisruptionBudgetList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						pl := list.(*policyv1.PodDisruptionBudgetList)
						pl.Items = []policyv1.PodDisruptionBudget{
							{
								ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-logging", Name: "fluentd-pdb"},
								Spec: policyv1.PodDisruptionBudgetSpec{
									MaxUnavailable: &zero,
									Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "fluentd"}},
								},
							},
						}
						return nil
					})
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
				Expect(output.Findings).To(ContainElement(ContainSubstring("fluentd-pdb")))
			})

			It("returns Failed when a DaemonSet with wildcard toleration is covered by a restrictive PDB", func() {
				deps := CheckDependencies{Client: mockClient}
				zero := intstr.FromInt(0)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.PodList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&appsv1.DaemonSetList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						dl := list.(*appsv1.DaemonSetList)
						dl.Items = []appsv1.DaemonSet{
							dsWithTolerations("kube-system", "kube-proxy", map[string]string{"app": "kube-proxy"}, []corev1.Toleration{wildcardToleration}),
						}
						return nil
					})
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&policyv1.PodDisruptionBudgetList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						pl := list.(*policyv1.PodDisruptionBudgetList)
						pl.Items = []policyv1.PodDisruptionBudget{
							{
								ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-proxy-pdb"},
								Spec: policyv1.PodDisruptionBudgetSpec{
									MaxUnavailable: &zero,
									Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "kube-proxy"}},
								},
							},
						}
						return nil
					})
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
				Expect(output.Findings).To(ContainElement(ContainSubstring("kube-proxy-pdb")))
			})

			It("does not flag a DaemonSet with no master/infra tolerations", func() {
				deps := CheckDependencies{Client: mockClient}
				workerOnlyToleration := corev1.Toleration{Key: "node-role.kubernetes.io/worker", Operator: corev1.TolerationOpExists}
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.PodList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&appsv1.DaemonSetList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						dl := list.(*appsv1.DaemonSetList)
						dl.Items = []appsv1.DaemonSet{
							dsWithTolerations("customer-ns", "worker-ds", map[string]string{"app": "worker-app"}, []corev1.Toleration{workerOnlyToleration}),
						}
						return nil
					})
				zero := intstr.FromInt(0)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&policyv1.PodDisruptionBudgetList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						pl := list.(*policyv1.PodDisruptionBudgetList)
						pl.Items = []policyv1.PodDisruptionBudget{
							{
								ObjectMeta: metav1.ObjectMeta{Namespace: "customer-ns", Name: "worker-pdb"},
								Spec: policyv1.PodDisruptionBudgetSpec{
									MaxUnavailable: &zero,
									Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker-app"}},
								},
							},
						}
						return nil
					})
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
				Expect(output.Findings).NotTo(ContainElement(ContainSubstring("worker-pdb")))
			})

			It("returns Error when node list fails", func() {
				deps := CheckDependencies{Client: mockClient}
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					Return(fmt.Errorf("list nodes failed"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
				Expect(output.Error).To(ContainSubstring("list nodes failed"))
			})

			It("returns Error when pod list fails", func() {
				deps := CheckDependencies{Client: mockClient}
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).
					DoAndReturn(func(_ context.Context, list interface{}, _ ...interface{}) error {
						nl := list.(*corev1.NodeList)
						nl.Items = []corev1.Node{masterNode("master-1")}
						return nil
					})
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.PodList{}), gomock.Any()).
					Return(fmt.Errorf("list pods failed"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
				Expect(output.Error).To(ContainSubstring("list pods failed"))
			})

			It("returns Error when DaemonSet list fails", func() {
				deps := CheckDependencies{Client: mockClient}
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.NodeList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&corev1.PodList{}), gomock.Any()).Return(nil)
				mockClient.EXPECT().List(gomock.Any(), gomock.AssignableToTypeOf(&appsv1.DaemonSetList{}), gomock.Any()).
					Return(fmt.Errorf("list daemonsets failed"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
				Expect(output.Error).To(ContainSubstring("list daemonsets failed"))
			})
		})

		Context("upgradeableConditionCheck", func() {
			var check *upgradeableConditionCheck
			BeforeEach(func() { check = &upgradeableConditionCheck{} })

			It("returns Passed when Upgradeable condition is True", func() {
				deps := CheckDependencies{Client: mockClient, CvClientBuilder: mockCvBuilder}
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
				mockCv.EXPECT().GetClusterVersion().Return(&configv1.ClusterVersion{
					Status: configv1.ClusterVersionStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{
								Type:   configv1.OperatorUpgradeable,
								Status: configv1.ConditionTrue,
							},
						},
					},
				}, nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Failed when Upgradeable condition is False", func() {
				deps := CheckDependencies{Client: mockClient, CvClientBuilder: mockCvBuilder}
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
				mockCv.EXPECT().GetClusterVersion().Return(&configv1.ClusterVersion{
					Status: configv1.ClusterVersionStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{
								Type:    configv1.OperatorUpgradeable,
								Status:  configv1.ConditionFalse,
								Reason:  "AdminAckRequired",
								Message: "Kubernetes 1.25 and therefore OpenShift 4.12 remove several APIs",
							},
						},
					},
				}, nil)
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
				Expect(output.Summary).To(ContainSubstring("AdminAckRequired"))
				Expect(output.Findings).To(ContainElement(ContainSubstring("AdminAckRequired")))
			})

			It("returns Error when GetClusterVersion fails", func() {
				deps := CheckDependencies{Client: mockClient, CvClientBuilder: mockCvBuilder}
				mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
				mockCv.EXPECT().GetClusterVersion().Return(nil, fmt.Errorf("cvo unavailable"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
				Expect(output.Error).To(ContainSubstring("cvo unavailable"))
			})
		})

		Context("pullSecretCheck", func() {
			var check *pullSecretCheck
			BeforeEach(func() { check = &pullSecretCheck{} })

			It("returns Passed when pull secret is valid", func() {
				deps := CheckDependencies{Client: mockClient}
				mockClient.EXPECT().Get(gomock.Any(), types.NamespacedName{Name: "version"}, gomock.AssignableToTypeOf(&configv1.ClusterVersion{})).
					Return(nil)
				mockClient.EXPECT().Get(gomock.Any(), types.NamespacedName{Namespace: "openshift-config", Name: "pull-secret"}, gomock.AssignableToTypeOf(&corev1.Secret{})).
					DoAndReturn(func(_ context.Context, _ types.NamespacedName, obj interface{}, _ ...interface{}) error {
						s := obj.(*corev1.Secret)
						s.Data = map[string][]byte{
							".dockerconfigjson": []byte(`{"auths":{"cloud.openshift.com":{"auth":"dGVzdA=="}}}`),
						}
						return nil
					})
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
			})

			It("returns Failed when pull secret is missing the required auth key", func() {
				deps := CheckDependencies{Client: mockClient}
				mockClient.EXPECT().Get(gomock.Any(), types.NamespacedName{Name: "version"}, gomock.AssignableToTypeOf(&configv1.ClusterVersion{})).
					Return(nil)
				mockClient.EXPECT().Get(gomock.Any(), types.NamespacedName{Namespace: "openshift-config", Name: "pull-secret"}, gomock.AssignableToTypeOf(&corev1.Secret{})).
					DoAndReturn(func(_ context.Context, _ types.NamespacedName, obj interface{}, _ ...interface{}) error {
						s := obj.(*corev1.Secret)
						s.Data = map[string][]byte{
							".dockerconfigjson": []byte(`{"auths":{"registry.redhat.io":{"auth":"dGVzdA=="}}}`),
						}
						return nil
					})
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
				Expect(output.Summary).To(ContainSubstring("Pull secret"))
			})

			It("returns Failed when pull secret Secret does not exist", func() {
				deps := CheckDependencies{Client: mockClient}
				mockClient.EXPECT().Get(gomock.Any(), types.NamespacedName{Name: "version"}, gomock.AssignableToTypeOf(&configv1.ClusterVersion{})).
					Return(nil)
				mockClient.EXPECT().Get(gomock.Any(), types.NamespacedName{Namespace: "openshift-config", Name: "pull-secret"}, gomock.AssignableToTypeOf(&corev1.Secret{})).
					Return(fmt.Errorf("cannot fetch pull secret: secret not found"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
			})
		})

		Context("adminAcksCheck", func() {
			var check *adminAcksCheck
			BeforeEach(func() { check = &adminAcksCheck{} })

			It("returns Passed when admin-acks ConfigMap exists with entries", func() {
				deps := CheckDependencies{Client: mockClient, DesiredVersion: instance.Spec.Desired.Version}
				mockClient.EXPECT().Get(gomock.Any(), types.NamespacedName{Namespace: "openshift-config", Name: "admin-acks"}, gomock.AssignableToTypeOf(&corev1.ConfigMap{})).
					DoAndReturn(func(_ context.Context, _ types.NamespacedName, obj interface{}, _ ...interface{}) error {
						cm := obj.(*corev1.ConfigMap)
						cm.Data = map[string]string{
							"ack-4.16-kube-1.29-api-removals-in-4.17": "true",
						}
						return nil
					})
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckPassed))
				Expect(output.Findings).To(HaveLen(1))
			})

			It("returns Failed when admin-acks ConfigMap is missing and target is set", func() {
				deps := CheckDependencies{Client: mockClient, DesiredVersion: instance.Spec.Desired.Version}
				mockClient.EXPECT().Get(gomock.Any(), types.NamespacedName{Namespace: "openshift-config", Name: "admin-acks"}, gomock.AssignableToTypeOf(&corev1.ConfigMap{})).
					Return(apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "admin-acks"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
				Expect(output.Summary).To(ContainSubstring("not found"))
			})

			It("returns Failed when admin-acks ConfigMap exists but is empty", func() {
				deps := CheckDependencies{Client: mockClient, DesiredVersion: instance.Spec.Desired.Version}
				mockClient.EXPECT().Get(gomock.Any(), types.NamespacedName{Namespace: "openshift-config", Name: "admin-acks"}, gomock.AssignableToTypeOf(&corev1.ConfigMap{})).
					DoAndReturn(func(_ context.Context, _ types.NamespacedName, obj interface{}, _ ...interface{}) error {
						cm := obj.(*corev1.ConfigMap)
						cm.Data = map[string]string{}
						return nil
					})
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckFailed))
				Expect(output.Summary).To(ContainSubstring("no entries"))
			})

			It("returns Error when ConfigMap read fails with non-NotFound error", func() {
				deps := CheckDependencies{Client: mockClient, DesiredVersion: instance.Spec.Desired.Version}
				mockClient.EXPECT().Get(gomock.Any(), types.NamespacedName{Namespace: "openshift-config", Name: "admin-acks"}, gomock.AssignableToTypeOf(&corev1.ConfigMap{})).
					Return(fmt.Errorf("unexpected API error"))
				output := check.Run(context.TODO(), deps, log, defaultCheckTimeout)
				Expect(output.Result).To(Equal(upgradev1alpha1.CheckResultError))
				Expect(output.Error).To(ContainSubstring("unexpected API error"))
			})
		})
	})

	Describe("runChecks with allowlist", func() {
		It("skips checks not in the allowlist and runs those that are", func() {
			instance.Spec.Checks = []string{"ClusterVersion"}

			mockCvBuilder.EXPECT().New(gomock.Any()).Return(mockCv)
			mockCv.EXPECT().GetClusterVersion().Return(&configv1.ClusterVersion{}, nil)

			results := reconciler.runChecks(context.TODO(), instance, log)
			Expect(results).To(HaveLen(len(registeredChecks)))

			var skipped, completed, errored int
			for _, r := range results {
				switch r.Execution.State {
				case upgradev1alpha1.CheckSkipped:
					skipped++
				case upgradev1alpha1.CheckCompleted:
					completed++
				case upgradev1alpha1.CheckError:
					errored++
				}
			}
			_ = errored
			Expect(completed).To(Equal(1))
			Expect(skipped).To(Equal(len(registeredChecks) - 1))
		})
	})
})
