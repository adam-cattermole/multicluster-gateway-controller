//go:build integration

package policy_integration

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	v1 "open-cluster-management.io/api/cluster/v1"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Kuadrant/multicluster-gateway-controller/pkg/apis/v1alpha1"
	"github.com/Kuadrant/multicluster-gateway-controller/pkg/dns"
	"github.com/Kuadrant/multicluster-gateway-controller/pkg/utils"
	testutil "github.com/Kuadrant/multicluster-gateway-controller/test/util"
)

var _ = Describe("DNSPolicy Health Checks", func() {

	var gatewayClass *gatewayapiv1.GatewayClass
	var managedZone *v1alpha1.ManagedZone
	var testNamespace string
	var dnsPolicyBuilder *testutil.DNSPolicyBuilder
	var gateway *gatewayapiv1.Gateway
	var dnsPolicy *v1alpha1.DNSPolicy
	var lbHash, recordName, wildcardRecordName string

	BeforeEach(func() {
		CreateNamespace(&testNamespace)

		gatewayClass = testutil.NewTestGatewayClass("foo", "default", "kuadrant.io/bar")
		Expect(k8sClient.Create(ctx, gatewayClass)).To(Succeed())

		managedZone = testutil.NewManagedZoneBuilder("mz-example-com", testNamespace, "example.com").ManagedZone
		Expect(k8sClient.Create(ctx, managedZone)).To(Succeed())

		gateway = testutil.NewGatewayBuilder(TestGatewayName, gatewayClass.Name, testNamespace).
			WithHTTPListener(TestListenerNameOne, TestHostOne).
			WithHTTPListener(TestListenerNameWildcard, TestHostWildcard).
			Gateway
		Expect(k8sClient.Create(ctx, gateway)).To(Succeed())

		//Set multi cluster gateway status
		Eventually(func() error {
			if err := k8sClient.Create(ctx, &v1.ManagedCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: TestClusterNameOne,
				},
			}); err != nil && !k8serrors.IsAlreadyExists(err) {
				return err
			}
			if err := k8sClient.Create(ctx, &v1.ManagedCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: TestClusterNameTwo,
				},
			}); err != nil && !k8serrors.IsAlreadyExists(err) {
				return err
			}
			gateway.Status.Addresses = []gatewayapiv1.GatewayStatusAddress{
				{
					Type:  testutil.Pointer(utils.MultiClusterIPAddressType),
					Value: TestClusterNameOne + "/" + TestIPAddressOne,
				},
				{
					Type:  testutil.Pointer(utils.MultiClusterIPAddressType),
					Value: TestClusterNameTwo + "/" + TestIPAddressTwo,
				},
			}
			gateway.Status.Listeners = []gatewayapiv1.ListenerStatus{
				{
					Name:           TestClusterNameOne + "." + TestListenerNameOne,
					SupportedKinds: []gatewayapiv1.RouteGroupKind{},
					AttachedRoutes: 1,
					Conditions:     []metav1.Condition{},
				},
				{
					Name:           TestClusterNameTwo + "." + TestListenerNameOne,
					SupportedKinds: []gatewayapiv1.RouteGroupKind{},
					AttachedRoutes: 1,
					Conditions:     []metav1.Condition{},
				},
				{
					Name:           TestClusterNameOne + "." + TestListenerNameWildcard,
					SupportedKinds: []gatewayapiv1.RouteGroupKind{},
					AttachedRoutes: 1,
					Conditions:     []metav1.Condition{},
				},
				{
					Name:           TestClusterNameTwo + "." + TestListenerNameWildcard,
					SupportedKinds: []gatewayapiv1.RouteGroupKind{},
					AttachedRoutes: 1,
					Conditions:     []metav1.Condition{},
				},
			}
			return k8sClient.Status().Update(ctx, gateway)
		}, TestTimeoutMedium, TestRetryIntervalMedium).ShouldNot(HaveOccurred())

		dnsPolicyBuilder = testutil.NewDNSPolicyBuilder("test-dns-policy", testNamespace)
		dnsPolicyBuilder.WithTargetGateway(TestGatewayName)

		lbHash = dns.ToBase36hash(fmt.Sprintf("%s-%s", gateway.Name, gateway.Namespace))
		recordName = fmt.Sprintf("%s-%s", TestGatewayName, TestListenerNameOne)
		wildcardRecordName = fmt.Sprintf("%s-%s", TestGatewayName, TestListenerNameWildcard)
	})

	AfterEach(func() {
		if gateway != nil {
			err := k8sClient.Delete(ctx, gateway)
			Expect(client.IgnoreNotFound(err)).ToNot(HaveOccurred())
		}
		if dnsPolicy != nil {
			err := k8sClient.Delete(ctx, dnsPolicy)
			Expect(client.IgnoreNotFound(err)).ToNot(HaveOccurred())

		}
		if managedZone != nil {
			err := k8sClient.Delete(ctx, managedZone)
			Expect(client.IgnoreNotFound(err)).ToNot(HaveOccurred())
		}
		if gatewayClass != nil {
			err := k8sClient.Delete(ctx, gatewayClass)
			Expect(client.IgnoreNotFound(err)).ToNot(HaveOccurred())
		}
	})

	Context("multi cluster gateway status", func() {

		Context("loadbalanced routing strategy", func() {

			BeforeEach(func() {
				dnsPolicyBuilder.WithRoutingStrategy(v1alpha1.LoadBalancedRoutingStrategy)
			})

			Context("with health checks", func() {
				var unhealthy bool

				BeforeEach(func() {
					dnsPolicyBuilder.
						WithLoadBalancingWeightedFor(120, nil).
						WithHealthCheckFor("/", nil, v1alpha1.HttpProtocol, testutil.Pointer(4))
					dnsPolicy = dnsPolicyBuilder.DNSPolicy
					Expect(k8sClient.Create(ctx, dnsPolicy)).To(BeNil())
					Eventually(func() error { //dns policy exists
						return k8sClient.Get(ctx, client.ObjectKey{Name: dnsPolicy.Name, Namespace: dnsPolicy.Namespace}, dnsPolicy)
					}, TestTimeoutMedium, TestRetryIntervalMedium).ShouldNot(HaveOccurred())
				})

				It("should create dns records", func() {
					Eventually(func(g Gomega, ctx context.Context) {
						recordList := &v1alpha1.DNSRecordList{}
						err := k8sClient.List(ctx, recordList, &client.ListOptions{Namespace: testNamespace})
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(recordList.Items).To(HaveLen(2))
						g.Expect(recordList.Items).To(
							ContainElements(
								MatchFields(IgnoreExtras, Fields{
									"ObjectMeta": HaveField("Name", recordName),
									"Spec": MatchFields(IgnoreExtras, Fields{
										"ManagedZoneRef": HaveField("Name", "mz-example-com"),
										"Endpoints":      HaveLen(6),
									}),
								}),
								MatchFields(IgnoreExtras, Fields{
									"ObjectMeta": HaveField("Name", wildcardRecordName),
									"Spec": MatchFields(IgnoreExtras, Fields{
										"ManagedZoneRef": HaveField("Name", "mz-example-com"),
										"Endpoints":      HaveLen(6),
									}),
								}),
							))
					}, TestTimeoutMedium, TestRetryIntervalMedium, ctx).Should(Succeed())
				})

				It("should have probes that are healthy", func() {
					probeList := &v1alpha1.DNSHealthCheckProbeList{}
					Eventually(func() error {
						Expect(k8sClient.List(ctx, probeList, &client.ListOptions{Namespace: testNamespace})).To(BeNil())
						if len(probeList.Items) != 2 {
							return fmt.Errorf("expected %v probes, got %v", 2, len(probeList.Items))
						}
						return nil
					}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeNil())
					Expect(len(probeList.Items)).To(Equal(2))
				})

				Context("all unhealthy probes", func() {
					It("should publish all dns records endpoints", func() {

						expectedEndpoints := []*v1alpha1.Endpoint{
							{
								DNSName: "2w705o.lb-" + lbHash + ".test.example.com",
								Targets: []string{
									TestIPAddressTwo,
								},
								RecordType:    "A",
								SetIdentifier: "",
								RecordTTL:     60,
							},
							{
								DNSName: "s07c46.lb-" + lbHash + ".test.example.com",
								Targets: []string{
									TestIPAddressOne,
								},
								RecordType:    "A",
								SetIdentifier: "",
								RecordTTL:     60,
							},
							{
								DNSName: "default.lb-" + lbHash + ".test.example.com",
								Targets: []string{
									"2w705o.lb-" + lbHash + ".test.example.com",
								},
								RecordType:    "CNAME",
								SetIdentifier: "2w705o.lb-" + lbHash + ".test.example.com",
								RecordTTL:     60,
								ProviderSpecific: v1alpha1.ProviderSpecific{
									{
										Name:  "weight",
										Value: "120",
									},
								},
							},
							{
								DNSName: "default.lb-" + lbHash + ".test.example.com",
								Targets: []string{
									"s07c46.lb-" + lbHash + ".test.example.com",
								},
								RecordType:    "CNAME",
								SetIdentifier: "s07c46.lb-" + lbHash + ".test.example.com",
								RecordTTL:     60,
								Labels:        nil,
								ProviderSpecific: v1alpha1.ProviderSpecific{
									{
										Name:  "weight",
										Value: "120",
									},
								},
							},
							{
								DNSName: "lb-" + lbHash + ".test.example.com",
								Targets: []string{
									"default.lb-" + lbHash + ".test.example.com",
								},
								RecordType:    "CNAME",
								SetIdentifier: "default",
								RecordTTL:     300,
								ProviderSpecific: v1alpha1.ProviderSpecific{
									{
										Name:  "geo-code",
										Value: "*",
									},
								},
							},
							{
								DNSName: "test.example.com",
								Targets: []string{
									"lb-" + lbHash + ".test.example.com",
								},
								RecordType:    "CNAME",
								SetIdentifier: "",
								RecordTTL:     300,
							},
						}

						probeList := &v1alpha1.DNSHealthCheckProbeList{}
						Eventually(func() error {
							Expect(k8sClient.List(ctx, probeList, &client.ListOptions{Namespace: testNamespace})).To(BeNil())
							if len(probeList.Items) != 2 {
								return fmt.Errorf("expected %v probes, got %v", 2, len(probeList.Items))
							}
							return nil
						}, TestTimeoutLong, TestRetryIntervalMedium).Should(BeNil())

						for _, probe := range probeList.Items {
							Eventually(func() error {
								if probe.Name == fmt.Sprintf("%s-%s-%s", TestIPAddressTwo, TestGatewayName, TestHostOne) ||
									probe.Name == fmt.Sprintf("%s-%s-%s", TestIPAddressOne, TestGatewayName, TestHostOne) {
									getProbe := &v1alpha1.DNSHealthCheckProbe{}
									if err := k8sClient.Get(ctx, client.ObjectKey{Name: probe.Name, Namespace: probe.Namespace}, getProbe); err != nil {
										return err
									}
									patch := client.MergeFrom(getProbe.DeepCopy())
									unhealthy = false
									getProbe.Status = v1alpha1.DNSHealthCheckProbeStatus{
										LastCheckedAt:       metav1.NewTime(time.Now()),
										ConsecutiveFailures: *getProbe.Spec.FailureThreshold + 1,
										Healthy:             &unhealthy,
									}
									if err := k8sClient.Status().Patch(ctx, getProbe, patch); err != nil {
										return err
									}
								}
								return nil
							}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeNil())
						}
						createdDNSRecord := &v1alpha1.DNSRecord{}
						Eventually(func() error {

							err := k8sClient.Get(ctx, client.ObjectKey{Name: recordName, Namespace: testNamespace}, createdDNSRecord)
							if err != nil && k8serrors.IsNotFound(err) {
								return err
							}
							if len(createdDNSRecord.Spec.Endpoints) != len(expectedEndpoints) {
								return fmt.Errorf("expected %v endpoints in DNSRecord, got %v", len(expectedEndpoints), len(createdDNSRecord.Spec.Endpoints))
							}
							return nil
						}, TestTimeoutLong, TestRetryIntervalMedium).Should(BeNil())
						Expect(createdDNSRecord.Spec.Endpoints).To(HaveLen(len(expectedEndpoints)))
						Expect(createdDNSRecord.Spec.Endpoints).Should(ContainElements(expectedEndpoints))
					})
				})
				Context("some unhealthy probes", func() {
					It("should publish expected endpoints", func() {

						expectedEndpoints := []*v1alpha1.Endpoint{
							{
								DNSName: "2w705o.lb-" + lbHash + ".test.example.com",
								Targets: []string{
									TestIPAddressTwo,
								},
								RecordType:    "A",
								SetIdentifier: "",
								RecordTTL:     60,
							},
							{
								DNSName: "s07c46.lb-" + lbHash + ".test.example.com",
								Targets: []string{
									TestIPAddressOne,
								},
								RecordType:    "A",
								SetIdentifier: "",
								RecordTTL:     60,
							},
							{
								DNSName: "default.lb-" + lbHash + ".test.example.com",
								Targets: []string{
									"2w705o.lb-" + lbHash + ".test.example.com",
								},
								RecordType:    "CNAME",
								SetIdentifier: "2w705o.lb-" + lbHash + ".test.example.com",
								RecordTTL:     60,
								ProviderSpecific: v1alpha1.ProviderSpecific{
									{
										Name:  "weight",
										Value: "120",
									},
								},
							},
							{
								DNSName: "default.lb-" + lbHash + ".test.example.com",
								Targets: []string{
									"s07c46.lb-" + lbHash + ".test.example.com",
								},
								RecordType:    "CNAME",
								SetIdentifier: "s07c46.lb-" + lbHash + ".test.example.com",
								RecordTTL:     60,
								Labels:        nil,
								ProviderSpecific: v1alpha1.ProviderSpecific{
									{
										Name:  "weight",
										Value: "120",
									},
								},
							},
							{
								DNSName: "lb-" + lbHash + ".test.example.com",
								Targets: []string{
									"default.lb-" + lbHash + ".test.example.com",
								},
								RecordType:    "CNAME",
								SetIdentifier: "default",
								RecordTTL:     300,
								ProviderSpecific: v1alpha1.ProviderSpecific{
									{
										Name:  "geo-code",
										Value: "*",
									},
								},
							},
							{
								DNSName: "test.example.com",
								Targets: []string{
									"lb-" + lbHash + ".test.example.com",
								},
								RecordType:    "CNAME",
								SetIdentifier: "",
								RecordTTL:     300,
							},
						}

						probeList := &v1alpha1.DNSHealthCheckProbeList{}
						Eventually(func() error {
							Expect(k8sClient.List(ctx, probeList, &client.ListOptions{Namespace: testNamespace})).To(BeNil())
							if len(probeList.Items) != 2 {
								return fmt.Errorf("expected %v probes, got %v", 2, len(probeList.Items))
							}
							return nil
						}, TestTimeoutLong, TestRetryIntervalMedium).Should(BeNil())
						Expect(probeList.Items).To(HaveLen(2))

						Eventually(func() error {
							getProbe := &v1alpha1.DNSHealthCheckProbe{}
							if err := k8sClient.Get(ctx, client.ObjectKey{Name: fmt.Sprintf("%s-%s-%s", TestIPAddressOne, TestGatewayName, TestListenerNameOne), Namespace: testNamespace}, getProbe); err != nil {
								return err
							}
							patch := client.MergeFrom(getProbe.DeepCopy())
							unhealthy = false
							getProbe.Status = v1alpha1.DNSHealthCheckProbeStatus{
								LastCheckedAt:       metav1.NewTime(time.Now()),
								ConsecutiveFailures: *getProbe.Spec.FailureThreshold + 1,
								Healthy:             &unhealthy,
							}
							if err := k8sClient.Status().Patch(ctx, getProbe, patch); err != nil {
								return err
							}
							return nil
						}, TestTimeoutLong, TestRetryIntervalMedium).Should(BeNil())

						// after that verify that in time the endpoints are 5 in the dnsrecord
						createdDNSRecord := &v1alpha1.DNSRecord{}
						Eventually(func() error {
							err := k8sClient.Get(ctx, client.ObjectKey{Name: recordName, Namespace: testNamespace}, createdDNSRecord)
							if err != nil && k8serrors.IsNotFound(err) {
								return err
							}
							return nil
						}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeNil())
						Expect(createdDNSRecord.Spec.Endpoints).To(HaveLen(len(expectedEndpoints)))
						Expect(createdDNSRecord.Spec.Endpoints).Should(ContainElements(expectedEndpoints))
					})
				})
				Context("some unhealthy endpoints for other listener", func() {
					It("should publish expected endpoints", func() {

						expectedEndpoints := []*v1alpha1.Endpoint{
							{
								DNSName: "2w705o.lb-" + lbHash + ".test.example.com",
								Targets: []string{
									TestIPAddressTwo,
								},
								RecordType:    "A",
								SetIdentifier: "",
								RecordTTL:     60,
							},
							{
								DNSName: "s07c46.lb-" + lbHash + ".test.example.com",
								Targets: []string{
									TestIPAddressOne,
								},
								RecordType:    "A",
								SetIdentifier: "",
								RecordTTL:     60,
							},
							{
								DNSName: "default.lb-" + lbHash + ".test.example.com",
								Targets: []string{
									"2w705o.lb-" + lbHash + ".test.example.com",
								},
								RecordType:    "CNAME",
								SetIdentifier: "2w705o.lb-" + lbHash + ".test.example.com",
								RecordTTL:     60,
								ProviderSpecific: v1alpha1.ProviderSpecific{
									{
										Name:  "weight",
										Value: "120",
									},
								},
							},
							{
								DNSName: "default.lb-" + lbHash + ".test.example.com",
								Targets: []string{
									"s07c46.lb-" + lbHash + ".test.example.com",
								},
								RecordType:    "CNAME",
								SetIdentifier: "s07c46.lb-" + lbHash + ".test.example.com",
								RecordTTL:     60,
								Labels:        nil,
								ProviderSpecific: v1alpha1.ProviderSpecific{
									{
										Name:  "weight",
										Value: "120",
									},
								},
							},
							{
								DNSName: "lb-" + lbHash + ".test.example.com",
								Targets: []string{
									"default.lb-" + lbHash + ".test.example.com",
								},
								RecordType:    "CNAME",
								SetIdentifier: "default",
								RecordTTL:     300,
								ProviderSpecific: v1alpha1.ProviderSpecific{
									{
										Name:  "geo-code",
										Value: "*",
									},
								},
							},
							{
								DNSName: "test.example.com",
								Targets: []string{
									"lb-" + lbHash + ".test.example.com",
								},
								RecordType:    "CNAME",
								SetIdentifier: "",
								RecordTTL:     300,
							},
						}

						err := k8sClient.Get(ctx, client.ObjectKey{Name: gateway.Name, Namespace: gateway.Namespace}, gateway)
						Expect(err).NotTo(HaveOccurred())
						Expect(gateway.Spec.Listeners).NotTo(BeNil())
						// add another listener, should result in 4 probes
						typedHostname := gatewayapiv1.Hostname(TestHostTwo)
						otherListener := gatewayapiv1.Listener{
							Name:     gatewayapiv1.SectionName(TestListenerNameTwo),
							Hostname: &typedHostname,
							Port:     gatewayapiv1.PortNumber(80),
							Protocol: gatewayapiv1.HTTPProtocolType,
						}

						patch := client.MergeFrom(gateway.DeepCopy())
						gateway.Spec.Listeners = append(gateway.Spec.Listeners, otherListener)
						Expect(k8sClient.Patch(ctx, gateway, patch)).To(BeNil())

						probeList := &v1alpha1.DNSHealthCheckProbeList{}
						Eventually(func() error {
							Expect(k8sClient.List(ctx, probeList, &client.ListOptions{Namespace: testNamespace})).To(BeNil())
							if len(probeList.Items) != 4 {
								return fmt.Errorf("expected %v probes, got %v", 4, len(probeList.Items))
							}
							return nil
						}, TestTimeoutLong, TestRetryIntervalMedium).Should(BeNil())
						Expect(len(probeList.Items)).To(Equal(4))

						//
						Eventually(func() error {
							getProbe := &v1alpha1.DNSHealthCheckProbe{}
							if err = k8sClient.Get(ctx, client.ObjectKey{Name: fmt.Sprintf("%s-%s-%s", TestIPAddressOne, TestGatewayName, TestListenerNameTwo), Namespace: testNamespace}, getProbe); err != nil {
								return err
							}
							patch := client.MergeFrom(getProbe.DeepCopy())
							unhealthy = false
							getProbe.Status = v1alpha1.DNSHealthCheckProbeStatus{
								LastCheckedAt:       metav1.NewTime(time.Now()),
								ConsecutiveFailures: *getProbe.Spec.FailureThreshold + 1,
								Healthy:             &unhealthy,
							}
							if err = k8sClient.Status().Patch(ctx, getProbe, patch); err != nil {
								return err
							}
							return nil
						}, TestTimeoutLong, TestRetryIntervalMedium).Should(BeNil())

						// after that verify that in time the endpoints are 5 in the dnsrecord
						createdDNSRecord := &v1alpha1.DNSRecord{}
						Eventually(func() error {
							err := k8sClient.Get(ctx, client.ObjectKey{Name: recordName, Namespace: testNamespace}, createdDNSRecord)
							if err != nil && k8serrors.IsNotFound(err) {
								return err
							}
							return nil
						}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeNil())
						Expect(createdDNSRecord.Spec.Endpoints).To(HaveLen(len(expectedEndpoints)))
						Expect(createdDNSRecord.Spec.Endpoints).Should(ContainElements(expectedEndpoints))
					})
				})
			})

		})

	})

})
