//go:build integration

package policy_integration

import (
	"context"
	"fmt"

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

var _ = Describe("DNSPolicy Multi Cluster", func() {

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

	Context("simple routing strategy", func() {

		BeforeEach(func() {
			dnsPolicyBuilder.WithRoutingStrategy(v1alpha1.SimpleRoutingStrategy)
			dnsPolicy = dnsPolicyBuilder.DNSPolicy
			Expect(k8sClient.Create(ctx, dnsPolicy)).To(Succeed())
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
								"Endpoints": ConsistOf(
									PointTo(MatchFields(IgnoreExtras, Fields{
										"DNSName":       Equal(TestHostOne),
										"Targets":       ContainElements(TestIPAddressOne, TestIPAddressTwo),
										"RecordType":    Equal("A"),
										"SetIdentifier": Equal(""),
										"RecordTTL":     Equal(v1alpha1.TTL(60)),
									})),
								),
							}),
						}),
						MatchFields(IgnoreExtras, Fields{
							"ObjectMeta": HaveField("Name", wildcardRecordName),
							"Spec": MatchFields(IgnoreExtras, Fields{
								"ManagedZoneRef": HaveField("Name", "mz-example-com"),
								"Endpoints": ConsistOf(
									PointTo(MatchFields(IgnoreExtras, Fields{
										"DNSName":       Equal(TestHostWildcard),
										"Targets":       ContainElements(TestIPAddressOne, TestIPAddressTwo),
										"RecordType":    Equal("A"),
										"SetIdentifier": Equal(""),
										"RecordTTL":     Equal(v1alpha1.TTL(60)),
									})),
								),
							}),
						}),
					))
			}, TestTimeoutMedium, TestRetryIntervalMedium, ctx).Should(Succeed())
		})

	})

	Context("loadbalanced routing strategy", func() {

		BeforeEach(func() {
			dnsPolicyBuilder.WithRoutingStrategy(v1alpha1.LoadBalancedRoutingStrategy)
		})

		Context("weighted", func() {

			BeforeEach(func() {
				dnsPolicyBuilder.WithLoadBalancingWeightedFor(120, nil)
				dnsPolicy = dnsPolicyBuilder.DNSPolicy
				Expect(k8sClient.Create(ctx, dnsPolicy)).To(Succeed())
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
									"Endpoints": ConsistOf(
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal("2w705o.lb-" + lbHash + ".test.example.com"),
											"Targets":       ConsistOf(TestIPAddressTwo),
											"RecordType":    Equal("A"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(60)),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("default.lb-" + lbHash + ".test.example.com"),
											"Targets":          ConsistOf("2w705o.lb-" + lbHash + ".test.example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("2w705o.lb-" + lbHash + ".test.example.com"),
											"RecordTTL":        Equal(v1alpha1.TTL(60)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "weight", Value: "120"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("default.lb-" + lbHash + ".test.example.com"),
											"Targets":          ConsistOf("s07c46.lb-" + lbHash + ".test.example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("s07c46.lb-" + lbHash + ".test.example.com"),
											"RecordTTL":        Equal(v1alpha1.TTL(60)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "weight", Value: "120"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal("s07c46.lb-" + lbHash + ".test.example.com"),
											"Targets":       ConsistOf(TestIPAddressOne),
											"RecordType":    Equal("A"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(60)),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("lb-" + lbHash + ".test.example.com"),
											"Targets":          ConsistOf("default.lb-" + lbHash + ".test.example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("default"),
											"RecordTTL":        Equal(v1alpha1.TTL(300)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "geo-code", Value: "*"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal(TestHostOne),
											"Targets":       ConsistOf("lb-" + lbHash + ".test.example.com"),
											"RecordType":    Equal("CNAME"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(300)),
										})),
									),
								}),
							}),
							MatchFields(IgnoreExtras, Fields{
								"ObjectMeta": HaveField("Name", wildcardRecordName),
								"Spec": MatchFields(IgnoreExtras, Fields{
									"ManagedZoneRef": HaveField("Name", "mz-example-com"),
									"Endpoints": ConsistOf(
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal("2w705o.lb-" + lbHash + ".example.com"),
											"Targets":       ConsistOf(TestIPAddressTwo),
											"RecordType":    Equal("A"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(60)),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("default.lb-" + lbHash + ".example.com"),
											"Targets":          ConsistOf("2w705o.lb-" + lbHash + ".example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("2w705o.lb-" + lbHash + ".example.com"),
											"RecordTTL":        Equal(v1alpha1.TTL(60)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "weight", Value: "120"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("default.lb-" + lbHash + ".example.com"),
											"Targets":          ConsistOf("s07c46.lb-" + lbHash + ".example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("s07c46.lb-" + lbHash + ".example.com"),
											"RecordTTL":        Equal(v1alpha1.TTL(60)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "weight", Value: "120"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal("s07c46.lb-" + lbHash + ".example.com"),
											"Targets":       ConsistOf(TestIPAddressOne),
											"RecordType":    Equal("A"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(60)),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("lb-" + lbHash + ".example.com"),
											"Targets":          ConsistOf("default.lb-" + lbHash + ".example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("default"),
											"RecordTTL":        Equal(v1alpha1.TTL(300)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "geo-code", Value: "*"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal(TestHostWildcard),
											"Targets":       ConsistOf("lb-" + lbHash + ".example.com"),
											"RecordType":    Equal("CNAME"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(300)),
										})),
									),
								}),
							}),
						))
				}, TestTimeoutMedium, TestRetryIntervalMedium, ctx).Should(Succeed())
			})

		})

		Context("geo+weighted", func() {

			BeforeEach(func() {
				dnsPolicyBuilder.
					WithLoadBalancingWeightedFor(120, nil).
					WithLoadBalancingGeoFor("IE")
				dnsPolicy = dnsPolicyBuilder.DNSPolicy
				Expect(k8sClient.Create(ctx, dnsPolicy)).To(Succeed())
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
									"Endpoints": ConsistOf(
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal("2w705o.lb-" + lbHash + ".test.example.com"),
											"Targets":       ConsistOf(TestIPAddressTwo),
											"RecordType":    Equal("A"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(60)),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("ie.lb-" + lbHash + ".test.example.com"),
											"Targets":          ConsistOf("2w705o.lb-" + lbHash + ".test.example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("2w705o.lb-" + lbHash + ".test.example.com"),
											"RecordTTL":        Equal(v1alpha1.TTL(60)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "weight", Value: "120"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("ie.lb-" + lbHash + ".test.example.com"),
											"Targets":          ConsistOf("s07c46.lb-" + lbHash + ".test.example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("s07c46.lb-" + lbHash + ".test.example.com"),
											"RecordTTL":        Equal(v1alpha1.TTL(60)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "weight", Value: "120"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal("s07c46.lb-" + lbHash + ".test.example.com"),
											"Targets":       ConsistOf(TestIPAddressOne),
											"RecordType":    Equal("A"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(60)),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("lb-" + lbHash + ".test.example.com"),
											"Targets":          ConsistOf("ie.lb-" + lbHash + ".test.example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("IE"),
											"RecordTTL":        Equal(v1alpha1.TTL(300)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "geo-code", Value: "IE"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("lb-" + lbHash + ".test.example.com"),
											"Targets":          ConsistOf("ie.lb-" + lbHash + ".test.example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("default"),
											"RecordTTL":        Equal(v1alpha1.TTL(300)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "geo-code", Value: "*"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal(TestHostOne),
											"Targets":       ConsistOf("lb-" + lbHash + ".test.example.com"),
											"RecordType":    Equal("CNAME"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(300)),
										})),
									),
								}),
							}),
							MatchFields(IgnoreExtras, Fields{
								"ObjectMeta": HaveField("Name", wildcardRecordName),
								"Spec": MatchFields(IgnoreExtras, Fields{
									"ManagedZoneRef": HaveField("Name", "mz-example-com"),
									"Endpoints": ConsistOf(
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal("2w705o.lb-" + lbHash + ".example.com"),
											"Targets":       ConsistOf(TestIPAddressTwo),
											"RecordType":    Equal("A"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(60)),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("ie.lb-" + lbHash + ".example.com"),
											"Targets":          ConsistOf("2w705o.lb-" + lbHash + ".example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("2w705o.lb-" + lbHash + ".example.com"),
											"RecordTTL":        Equal(v1alpha1.TTL(60)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "weight", Value: "120"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("ie.lb-" + lbHash + ".example.com"),
											"Targets":          ConsistOf("s07c46.lb-" + lbHash + ".example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("s07c46.lb-" + lbHash + ".example.com"),
											"RecordTTL":        Equal(v1alpha1.TTL(60)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "weight", Value: "120"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal("s07c46.lb-" + lbHash + ".example.com"),
											"Targets":       ConsistOf(TestIPAddressOne),
											"RecordType":    Equal("A"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(60)),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("lb-" + lbHash + ".example.com"),
											"Targets":          ConsistOf("ie.lb-" + lbHash + ".example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("IE"),
											"RecordTTL":        Equal(v1alpha1.TTL(300)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "geo-code", Value: "IE"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("lb-" + lbHash + ".example.com"),
											"Targets":          ConsistOf("ie.lb-" + lbHash + ".example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("default"),
											"RecordTTL":        Equal(v1alpha1.TTL(300)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "geo-code", Value: "*"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal(TestHostWildcard),
											"Targets":       ConsistOf("lb-" + lbHash + ".example.com"),
											"RecordType":    Equal("CNAME"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(300)),
										})),
									),
								}),
							}),
						))
				}, TestTimeoutMedium, TestRetryIntervalMedium, ctx).Should(Succeed())
			})

		})

		Context("geo+weighted with custom weights", func() {

			BeforeEach(func() {
				dnsPolicyBuilder.
					WithLoadBalancingWeightedFor(120, []*v1alpha1.CustomWeight{
						{
							Selector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"kuadrant.io/my-custom-weight-attr": "FOO",
								},
							},
							Weight: 100,
						},
						{
							Selector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"kuadrant.io/my-custom-weight-attr": "BAR",
								},
							},
							Weight: 160,
						},
					}).
					WithLoadBalancingGeoFor("IE")
				dnsPolicy = dnsPolicyBuilder.DNSPolicy
				Expect(k8sClient.Create(ctx, dnsPolicy)).To(Succeed())

				Eventually(func() error {
					gateway.Labels = map[string]string{}
					gateway.Labels["clusters.kuadrant.io/"+TestClusterNameOne+"_my-custom-weight-attr"] = "FOO"
					gateway.Labels["clusters.kuadrant.io/"+TestClusterNameTwo+"_my-custom-weight-attr"] = "BAR"
					gateway.Labels["clusters.kuadrant.io/"+TestClusterNameOne+"_lb-attribute-geo-code"] = "IE"
					gateway.Labels["clusters.kuadrant.io/"+TestClusterNameTwo+"_lb-attribute-geo-code"] = "ES"
					return k8sClient.Update(ctx, gateway)
				}, TestTimeoutMedium, TestRetryIntervalMedium).ShouldNot(HaveOccurred())

				Expect(gateway.Labels).To(HaveKeyWithValue("clusters.kuadrant.io/test-placed-control_my-custom-weight-attr", "FOO"))
				Expect(gateway.Labels).To(HaveKeyWithValue("clusters.kuadrant.io/test-placed-control_lb-attribute-geo-code", "IE"))
				Expect(gateway.Labels).To(HaveKeyWithValue("clusters.kuadrant.io/test-placed-workload-1_my-custom-weight-attr", "BAR"))
				Expect(gateway.Labels).To(HaveKeyWithValue("clusters.kuadrant.io/test-placed-workload-1_lb-attribute-geo-code", "ES"))
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
									"Endpoints": ConsistOf(
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal("2w705o.lb-" + lbHash + ".test.example.com"),
											"Targets":       ConsistOf(TestIPAddressTwo),
											"RecordType":    Equal("A"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(60)),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("es.lb-" + lbHash + ".test.example.com"),
											"Targets":          ConsistOf("2w705o.lb-" + lbHash + ".test.example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("2w705o.lb-" + lbHash + ".test.example.com"),
											"RecordTTL":        Equal(v1alpha1.TTL(60)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "weight", Value: "160"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("ie.lb-" + lbHash + ".test.example.com"),
											"Targets":          ConsistOf("s07c46.lb-" + lbHash + ".test.example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("s07c46.lb-" + lbHash + ".test.example.com"),
											"RecordTTL":        Equal(v1alpha1.TTL(60)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "weight", Value: "100"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal("s07c46.lb-" + lbHash + ".test.example.com"),
											"Targets":       ConsistOf(TestIPAddressOne),
											"RecordType":    Equal("A"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(60)),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("lb-" + lbHash + ".test.example.com"),
											"Targets":          ConsistOf("es.lb-" + lbHash + ".test.example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("ES"),
											"RecordTTL":        Equal(v1alpha1.TTL(300)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "geo-code", Value: "ES"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("lb-" + lbHash + ".test.example.com"),
											"Targets":          ConsistOf("ie.lb-" + lbHash + ".test.example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("IE"),
											"RecordTTL":        Equal(v1alpha1.TTL(300)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "geo-code", Value: "IE"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("lb-" + lbHash + ".test.example.com"),
											"Targets":          ConsistOf("ie.lb-" + lbHash + ".test.example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("default"),
											"RecordTTL":        Equal(v1alpha1.TTL(300)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "geo-code", Value: "*"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal(TestHostOne),
											"Targets":       ConsistOf("lb-" + lbHash + ".test.example.com"),
											"RecordType":    Equal("CNAME"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(300)),
										})),
									),
								}),
							}),
							MatchFields(IgnoreExtras, Fields{
								"ObjectMeta": HaveField("Name", wildcardRecordName),
								"Spec": MatchFields(IgnoreExtras, Fields{
									"ManagedZoneRef": HaveField("Name", "mz-example-com"),
									"Endpoints": ConsistOf(
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal("2w705o.lb-" + lbHash + ".example.com"),
											"Targets":       ConsistOf(TestIPAddressTwo),
											"RecordType":    Equal("A"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(60)),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("es.lb-" + lbHash + ".example.com"),
											"Targets":          ConsistOf("2w705o.lb-" + lbHash + ".example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("2w705o.lb-" + lbHash + ".example.com"),
											"RecordTTL":        Equal(v1alpha1.TTL(60)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "weight", Value: "160"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("ie.lb-" + lbHash + ".example.com"),
											"Targets":          ConsistOf("s07c46.lb-" + lbHash + ".example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("s07c46.lb-" + lbHash + ".example.com"),
											"RecordTTL":        Equal(v1alpha1.TTL(60)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "weight", Value: "100"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal("s07c46.lb-" + lbHash + ".example.com"),
											"Targets":       ConsistOf(TestIPAddressOne),
											"RecordType":    Equal("A"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(60)),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("lb-" + lbHash + ".example.com"),
											"Targets":          ConsistOf("ie.lb-" + lbHash + ".example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("IE"),
											"RecordTTL":        Equal(v1alpha1.TTL(300)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "geo-code", Value: "IE"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("lb-" + lbHash + ".example.com"),
											"Targets":          ConsistOf("es.lb-" + lbHash + ".example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("ES"),
											"RecordTTL":        Equal(v1alpha1.TTL(300)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "geo-code", Value: "ES"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":          Equal("lb-" + lbHash + ".example.com"),
											"Targets":          ConsistOf("ie.lb-" + lbHash + ".example.com"),
											"RecordType":       Equal("CNAME"),
											"SetIdentifier":    Equal("default"),
											"RecordTTL":        Equal(v1alpha1.TTL(300)),
											"ProviderSpecific": Equal(v1alpha1.ProviderSpecific{{Name: "geo-code", Value: "*"}}),
										})),
										PointTo(MatchFields(IgnoreExtras, Fields{
											"DNSName":       Equal(TestHostWildcard),
											"Targets":       ConsistOf("lb-" + lbHash + ".example.com"),
											"RecordType":    Equal("CNAME"),
											"SetIdentifier": Equal(""),
											"RecordTTL":     Equal(v1alpha1.TTL(300)),
										})),
									),
								}),
							}),
						))
				}, TestTimeoutMedium, TestRetryIntervalMedium, ctx).Should(Succeed())
			})

		})

	})

})
