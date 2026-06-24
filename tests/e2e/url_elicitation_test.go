//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	elicitationExtName     = "elicitation-ext"
	elicitationNamespace   = "mcp-elicitation"
	elicitationDeployment  = "mcp-gateway"
	elicitationTLSHostname = "*.mcp-gateway.local"
	elicitationTLSCertName = "mcp-gateway-tls-cert"
	elicitationPublicHost  = "elicit.mcp-gateway.local"
)

var _ = Describe("URL Elicitation", Ordered, ContinueOnFailure, func() {
	var (
		testResources  []client.Object
		prefix         string
		elicitationExt *MCPGatewayExtensionSetup
	)

	BeforeAll(func() {
		By("Creating elicitation namespace")
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   elicitationNamespace,
				Labels: map[string]string{"e2e": "test"},
			},
		}
		_ = k8sClient.Delete(ctx, ns)
		Eventually(func(g Gomega) {
			err := k8sClient.Create(ctx, ns)
			g.Expect(client.IgnoreAlreadyExists(err)).NotTo(HaveOccurred())
		}, TestTimeoutShort, TestRetryInterval).Should(Succeed())

		By("Removing stale HTTPS listener if present from a prior run")
		removeGatewayListener(ctx, k8sClient, GatewayNamespace, ElicitationGatewayName, ElicitationListenerName)

		By("Adding HTTPS listener to elicitation gateway")
		Expect(AddGatewayHTTPSListener(ctx, GatewayNamespace, ElicitationGatewayName,
			ElicitationListenerName, elicitationTLSHostname, elicitationTLSCertName, 8443)).To(Succeed())

		By("Creating MCPGatewayExtension with URL elicitation enabled")
		elicitationExt = NewMCPGatewayExtensionSetup(k8sClient).
			WithName(elicitationExtName).
			InNamespace(elicitationNamespace).
			TargetingGateway(ElicitationGatewayName, GatewayNamespace).
			WithSectionName(ElicitationListenerName).
			WithPublicHost(elicitationPublicHost).
			WithListenerPort(8443).
			WithURLElicitation().
			Build()

		elicitationExt.Clean(ctx).Register(ctx)

		By("Waiting for MCPGatewayExtension to become ready")
		Eventually(func(g Gomega) {
			err := VerifyMCPGatewayExtensionReady(ctx, k8sClient, elicitationExtName, elicitationNamespace)
			g.Expect(err).NotTo(HaveOccurred())
		}, TestTimeoutMedium, TestRetryInterval).Should(Succeed())

		By("Patching broker-router with CA cert for HTTPS")
		caSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "private-ca-keypair", Namespace: "cert-manager"}, caSecret)).To(Succeed())
		caCertPEM, ok := caSecret.Data["ca.crt"]
		Expect(ok).To(BeTrue())

		caBundle := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "elicitation-ca-bundle",
				Namespace: elicitationNamespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"ca.crt": caCertPEM},
		}
		_ = k8sClient.Delete(ctx, caBundle)
		Expect(k8sClient.Create(ctx, caBundle)).To(Succeed())

		combinedPatch := `[` +
			`{"op":"add","path":"/spec/template/spec/volumes/-","value":{"name":"gateway-ca","secret":{"secretName":"elicitation-ca-bundle"}}},` +
			`{"op":"add","path":"/spec/template/spec/containers/0/volumeMounts/-","value":{"name":"gateway-ca","mountPath":"/certs/gateway-ca.crt","subPath":"ca.crt","readOnly":true}},` +
			`{"op":"add","path":"/spec/template/spec/containers/0/command/-","value":"--gateway-ca-cert=/certs/gateway-ca.crt"}` +
			`]`
		Expect(PatchDeploymentJSON(ctx, elicitationNamespace, elicitationDeployment, combinedPatch)).To(Succeed())
		Expect(WaitForDeploymentReady(ctx, elicitationNamespace, elicitationDeployment)).To(Succeed())
	})

	AfterAll(func() {
		By("Tearing down elicitation gateway infrastructure")
		if elicitationExt != nil {
			elicitationExt.TearDown(ctx)
		}

		caBundle := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "elicitation-ca-bundle", Namespace: elicitationNamespace},
		}
		_ = k8sClient.Delete(ctx, caBundle)
		_ = RemoveDeploymentCommandFlag(ctx, elicitationNamespace, elicitationDeployment, "--gateway-ca-cert=/certs/gateway-ca.crt")
		_ = RemoveDeploymentVolumeMount(ctx, elicitationNamespace, elicitationDeployment, "gateway-ca")
		_ = RemoveDeploymentVolume(ctx, elicitationNamespace, elicitationDeployment, "gateway-ca")
	})

	BeforeEach(func() {
		By("Pre-cleaning credential secret from prior runs")
		cred := BuildCredentialSecret("url-elicit-cred", "test-api-key-secret-token")
		CleanupResource(ctx, k8sClient, cred)

		By("Registering api-key-server with tokenURLElicitation and credentialRef")
		cred = BuildCredentialSecret("url-elicit-cred", "test-api-key-secret-token")
		registration := NewMCPServerResourcesWithDefaults("urlelicit", k8sClient).
			WithCredential(cred, "token").
			WithBackendTarget("mcp-api-key-server", 9090).
			WithParentGateway(ElicitationGatewayName, GatewayNamespace).
			WithHostname("elicit-apikey.mcp-gateway.local").
			WithPrefix("ue_").
			WithTokenURLElicitation("").
			Build()
		testResources = append(testResources, registration.GetObjects()...)
		registeredServer := registration.Register(ctx)
		prefix = registeredServer.Spec.Prefix

		By("Waiting for the server to become ready")
		Eventually(func(g Gomega) {
			g.Expect(VerifyMCPServerRegistrationReady(ctx, k8sClient, registeredServer.Name, registeredServer.Namespace)).To(BeNil())
		}, TestTimeoutConfigSync, TestRetryInterval).To(Succeed())
	})

	AfterEach(func() {
		for _, to := range testResources {
			CleanupResource(ctx, k8sClient, to)
		}
		testResources = nil
	})

	It("[Happy,URLElicitation] URL elicitation triggers on missing token for elicitation-capable client; server without tokenURLElicitation is unaffected", func() {
		toolName := fmt.Sprintf("%shello_world", prefix)

		By("Registering a second server WITHOUT tokenURLElicitation or credentialRef")
		registration2 := NewMCPServerResourcesWithDefaults("urlelicit-nocfg", k8sClient).
			WithBackendTarget(sharedMCPTestServer1, 9090).
			WithParentGateway(ElicitationGatewayName, GatewayNamespace).
			WithHostname("elicit-server1.mcp-gateway.local").
			WithPrefix("uenone_").
			Build()
		testResources = append(testResources, registration2.GetObjects()...)
		registeredServer2 := registration2.Register(ctx)
		toolName2 := fmt.Sprintf("%sgreet", registeredServer2.Spec.Prefix)

		Eventually(func(g Gomega) {
			g.Expect(VerifyMCPServerRegistrationReady(ctx, k8sClient, registeredServer2.Name, registeredServer2.Namespace)).To(BeNil())
		}, TestTimeoutConfigSync, TestRetryInterval).To(Succeed())

		By("Initializing with elicitation capability")
		var sessionID string
		Eventually(func(g Gomega) {
			var err error
			sessionID, err = mcpInitializeWithElicitation(ElicitationGatewayURL, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(sessionID).NotTo(BeEmpty())
		}, TestTimeoutMedium, TestRetryInterval).Should(Succeed())

		Expect(mcpNotifyInitialized(context.Background(), ElicitationGatewayURL, sessionID, nil)).To(Succeed())

		By("Waiting for tools from both servers to be available")
		Eventually(func(g Gomega) {
			_, tools, err := mcpListTools(context.Background(), ElicitationGatewayURL, sessionID, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(tools).To(ContainElement(toolName))
			g.Expect(tools).To(ContainElement(toolName2))
		}, TestTimeoutLong, TestRetryInterval).Should(Succeed())

		By("Calling tool on the elicitation server — should get -32042 with elicitation URL")
		status, body, _, err := mcpCallToolRaw(ElicitationGatewayURL, sessionID, toolName, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200))

		sseErr, err := parseSSEError(body)
		Expect(err).NotTo(HaveOccurred())
		Expect(sseErr.Code).To(Equal(-32042))
		Expect(sseErr.Message).To(Equal("URL elicitation required"))

		elicitURL, err := extractElicitationURL(sseErr)
		Expect(err).NotTo(HaveOccurred())
		Expect(elicitURL).To(ContainSubstring("elicitation_id="))

		By("Calling tool on the server without tokenURLElicitation — should succeed without elicitation, no -32042")
		Eventually(func(g Gomega) {
			directStatus, directContent, err := mcpCallTool(context.Background(), ElicitationGatewayURL, sessionID, toolName2, map[string]any{"name": "direct"}, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(directStatus).To(Equal(200))
			g.Expect(directContent).NotTo(BeEmpty())
			g.Expect(directContent[0].Text).To(ContainSubstring("Hi direct"))
		}, TestTimeoutMedium, TestRetryInterval).Should(Succeed())
	})

	It("[Happy,URLElicitation] Full round-trip: token page submit then retry succeeds", func() {
		toolName := fmt.Sprintf("%shello_world", prefix)

		By("Initializing with elicitation capability")
		var sessionID string
		Eventually(func(g Gomega) {
			var err error
			sessionID, err = mcpInitializeWithElicitation(ElicitationGatewayURL, nil)
			g.Expect(err).NotTo(HaveOccurred())
		}, TestTimeoutMedium, TestRetryInterval).Should(Succeed())

		Expect(mcpNotifyInitialized(context.Background(), ElicitationGatewayURL, sessionID, nil)).To(Succeed())

		By("Waiting for tools to be available")
		Eventually(func(g Gomega) {
			_, tools, err := mcpListTools(context.Background(), ElicitationGatewayURL, sessionID, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(tools).To(ContainElement(toolName))
		}, TestTimeoutLong, TestRetryInterval).Should(Succeed())

		By("Calling tool — should get -32042")
		_, body, _, err := mcpCallToolRaw(ElicitationGatewayURL, sessionID, toolName, nil, nil)
		Expect(err).NotTo(HaveOccurred())

		sseErr, err := parseSSEError(body)
		Expect(err).NotTo(HaveOccurred())
		Expect(sseErr.Code).To(Equal(-32042))

		elicitURL, err := extractElicitationURL(sseErr)
		Expect(err).NotTo(HaveOccurred())

		By("Adapting URL for test environment")
		testURL, err := adaptElicitationURL(elicitURL, ElicitationGatewayURL)
		Expect(err).NotTo(HaveOccurred())
		GinkgoWriter.Println("token page URL:", testURL)

		By("GET /tokens — should return form page with CSRF token")
		status, htmlBody, cookies, err := rawHTTPGetFull(testURL, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200))
		Expect(htmlBody).To(ContainSubstring("elicitation_id"))

		csrfToken := extractHiddenField(htmlBody, "csrf_token")
		Expect(csrfToken).NotTo(BeEmpty(), "csrf_token hidden field not found in form")

		By("Extracting elicitation_id from URL")
		parsed, err := url.Parse(testURL)
		Expect(err).NotTo(HaveOccurred())
		elicitationID := parsed.Query().Get("elicitation_id")

		By("POST /tokens — submit token with CSRF cookie and token")
		formValues := url.Values{
			"elicitation_id": {elicitationID},
			"token":          {"Bearer test-api-key-secret-token"},
			"csrf_token":     {csrfToken},
		}
		postStatus, _, err := rawHTTPPostForm(
			strings.TrimSuffix(ElicitationGatewayURL, "/mcp")+"/tokens",
			formValues,
			nil,
			cookies...,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(postStatus).To(Equal(200))

		By("Retrying tool call — should succeed now")
		Eventually(func(g Gomega) {
			retryStatus, retryContent, err := mcpCallTool(context.Background(), ElicitationGatewayURL, sessionID, toolName, map[string]any{"name": "e2e"}, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(retryStatus).To(Equal(200))
			g.Expect(retryContent).NotTo(BeEmpty())
			g.Expect(retryContent[0].Text).To(ContainSubstring("Hello"))
		}, TestTimeoutMedium, TestRetryInterval).Should(Succeed())
	})

	It("[Full][URLElicitation] Cached token reused across multiple tool calls", func() {
		toolName := fmt.Sprintf("%shello_world", prefix)

		By("Initializing, triggering -32042, and submitting token")
		var sessionID string
		Eventually(func(g Gomega) {
			var err error
			sessionID, err = mcpInitializeWithElicitation(ElicitationGatewayURL, nil)
			g.Expect(err).NotTo(HaveOccurred())
		}, TestTimeoutMedium, TestRetryInterval).Should(Succeed())

		Expect(mcpNotifyInitialized(context.Background(), ElicitationGatewayURL, sessionID, nil)).To(Succeed())

		Eventually(func(g Gomega) {
			_, tools, err := mcpListTools(context.Background(), ElicitationGatewayURL, sessionID, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(tools).To(ContainElement(toolName))
		}, TestTimeoutLong, TestRetryInterval).Should(Succeed())

		_, body, _, err := mcpCallToolRaw(ElicitationGatewayURL, sessionID, toolName, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		sseErr, err := parseSSEError(body)
		Expect(err).NotTo(HaveOccurred())
		Expect(sseErr.Code).To(Equal(-32042))

		elicitURL, err := extractElicitationURL(sseErr)
		Expect(err).NotTo(HaveOccurred())
		testURL, err := adaptElicitationURL(elicitURL, ElicitationGatewayURL)
		Expect(err).NotTo(HaveOccurred())

		_, htmlBody, cookies, err := rawHTTPGetFull(testURL, nil)
		Expect(err).NotTo(HaveOccurred())
		csrfToken := extractHiddenField(htmlBody, "csrf_token")
		Expect(csrfToken).NotTo(BeEmpty())

		parsed, err := url.Parse(testURL)
		Expect(err).NotTo(HaveOccurred())
		elicitationID := parsed.Query().Get("elicitation_id")

		formValues := url.Values{
			"elicitation_id": {elicitationID},
			"token":          {"Bearer test-api-key-secret-token"},
			"csrf_token":     {csrfToken},
		}
		postStatus, _, postErr := rawHTTPPostForm(
			strings.TrimSuffix(ElicitationGatewayURL, "/mcp")+"/tokens",
			formValues,
			nil,
			cookies...,
		)
		Expect(postErr).NotTo(HaveOccurred())
		Expect(postStatus).To(Equal(200))

		By("First tool call should succeed with cached token")
		Eventually(func(g Gomega) {
			status1, content1, err := mcpCallTool(context.Background(), ElicitationGatewayURL, sessionID, toolName, map[string]any{"name": "call1"}, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status1).To(Equal(200))
			g.Expect(content1).NotTo(BeEmpty())
		}, TestTimeoutMedium, TestRetryInterval).Should(Succeed())

		By("Second tool call should also succeed — no new -32042")
		status2, content2, err := mcpCallTool(context.Background(), ElicitationGatewayURL, sessionID, toolName, map[string]any{"name": "call2"}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(status2).To(Equal(200))
		Expect(content2).NotTo(BeEmpty())
		Expect(content2[0].Text).To(ContainSubstring("Hello"))
	})

	It("[URLElicitation] Non-elicitation-capable client gets standard error on missing token", func() {
		toolName := fmt.Sprintf("%shello_world", prefix)

		By("Initializing WITHOUT elicitation capability")
		var sessionID string
		Eventually(func(g Gomega) {
			var err error
			sessionID, err = mcpInitialize(context.Background(), ElicitationGatewayURL, nil)
			g.Expect(err).NotTo(HaveOccurred())
		}, TestTimeoutMedium, TestRetryInterval).Should(Succeed())

		Expect(mcpNotifyInitialized(context.Background(), ElicitationGatewayURL, sessionID, nil)).To(Succeed())

		By("Waiting for tools to be available")
		Eventually(func(g Gomega) {
			_, tools, err := mcpListTools(context.Background(), ElicitationGatewayURL, sessionID, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(tools).To(ContainElement(toolName))
		}, TestTimeoutLong, TestRetryInterval).Should(Succeed())

		By("Calling tool — should get an isError result, NOT -32042")
		status, body, _, err := mcpCallToolRaw(ElicitationGatewayURL, sessionID, toolName, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(200))
		Expect(body).To(ContainSubstring(`"isError":true`))
		Expect(body).To(ContainSubstring("elicitation"))
		Expect(body).NotTo(ContainSubstring("-32042"))
	})

	It("[Happy,URLElicitation] 401 from upstream invalidates cached token and re-triggers elicitation", func() {
		toolName := fmt.Sprintf("%shello_world", prefix)

		By("Initializing with elicitation capability")
		var sessionID string
		Eventually(func(g Gomega) {
			var err error
			sessionID, err = mcpInitializeWithElicitation(ElicitationGatewayURL, nil)
			g.Expect(err).NotTo(HaveOccurred())
		}, TestTimeoutMedium, TestRetryInterval).Should(Succeed())

		Expect(mcpNotifyInitialized(context.Background(), ElicitationGatewayURL, sessionID, nil)).To(Succeed())

		By("Waiting for tools to be available")
		Eventually(func(g Gomega) {
			_, tools, err := mcpListTools(context.Background(), ElicitationGatewayURL, sessionID, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(tools).To(ContainElement(toolName))
		}, TestTimeoutLong, TestRetryInterval).Should(Succeed())

		By("Calling tool — should get -32042 (no token yet)")
		_, body, _, err := mcpCallToolRaw(ElicitationGatewayURL, sessionID, toolName, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		sseErr, err := parseSSEError(body)
		Expect(err).NotTo(HaveOccurred())
		Expect(sseErr.Code).To(Equal(-32042))

		By("Submitting the CORRECT token via broker page")
		elicitURL, err := extractElicitationURL(sseErr)
		Expect(err).NotTo(HaveOccurred())
		testURL, err := adaptElicitationURL(elicitURL, ElicitationGatewayURL)
		Expect(err).NotTo(HaveOccurred())

		_, htmlBody, cookies, err := rawHTTPGetFull(testURL, nil)
		Expect(err).NotTo(HaveOccurred())
		csrfToken := extractHiddenField(htmlBody, "csrf_token")
		Expect(csrfToken).NotTo(BeEmpty())

		parsed, err := url.Parse(testURL)
		Expect(err).NotTo(HaveOccurred())
		elicitationID := parsed.Query().Get("elicitation_id")

		formValues := url.Values{
			"elicitation_id": {elicitationID},
			"token":          {"Bearer test-api-key-secret-token"},
			"csrf_token":     {csrfToken},
		}
		postStatus, _, postErr := rawHTTPPostForm(
			strings.TrimSuffix(ElicitationGatewayURL, "/mcp")+"/tokens",
			formValues,
			nil,
			cookies...,
		)
		Expect(postErr).NotTo(HaveOccurred())
		Expect(postStatus).To(Equal(200))

		By("Calling tool with correct token — should succeed and establish backend session")
		Eventually(func(g Gomega) {
			successStatus, successContent, err := mcpCallTool(context.Background(), ElicitationGatewayURL, sessionID, toolName, map[string]any{"name": "setup"}, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(successStatus).To(Equal(200))
			g.Expect(successContent).NotTo(BeEmpty())
			g.Expect(successContent[0].Text).To(ContainSubstring("Hello"))
		}, TestTimeoutMedium, TestRetryInterval).Should(Succeed())

		By("Calling tool with X-Force-Auth-Reject — upstream returns 401, gateway invalidates token")
		retryStatus, _, _, err := mcpCallToolRaw(ElicitationGatewayURL, sessionID, toolName, map[string]any{"name": "reject"}, map[string]string{"X-Force-Auth-Reject": "true"})
		Expect(err).NotTo(HaveOccurred())
		Expect(retryStatus).To(Equal(401))

		By("Retrying — should get -32042 (token was invalidated)")
		_, body2, _, err := mcpCallToolRaw(ElicitationGatewayURL, sessionID, toolName, map[string]any{"name": "retry"}, nil)
		Expect(err).NotTo(HaveOccurred())
		sseErr2, err := parseSSEError(body2)
		Expect(err).NotTo(HaveOccurred())
		Expect(sseErr2.Code).To(Equal(-32042))

		By("Submitting the correct token again")
		elicitURL2, err := extractElicitationURL(sseErr2)
		Expect(err).NotTo(HaveOccurred())
		testURL2, err := adaptElicitationURL(elicitURL2, ElicitationGatewayURL)
		Expect(err).NotTo(HaveOccurred())

		_, htmlBody2, cookies2, err := rawHTTPGetFull(testURL2, nil)
		Expect(err).NotTo(HaveOccurred())
		csrfToken2 := extractHiddenField(htmlBody2, "csrf_token")
		Expect(csrfToken2).NotTo(BeEmpty())

		parsed2, err := url.Parse(testURL2)
		Expect(err).NotTo(HaveOccurred())
		elicitationID2 := parsed2.Query().Get("elicitation_id")

		formValues2 := url.Values{
			"elicitation_id": {elicitationID2},
			"token":          {"Bearer test-api-key-secret-token"},
			"csrf_token":     {csrfToken2},
		}
		postStatus2, _, postErr2 := rawHTTPPostForm(
			strings.TrimSuffix(ElicitationGatewayURL, "/mcp")+"/tokens",
			formValues2,
			nil,
			cookies2...,
		)
		Expect(postErr2).NotTo(HaveOccurred())
		Expect(postStatus2).To(Equal(200))

		By("Final retry — should succeed with correct token")
		Eventually(func(g Gomega) {
			finalStatus, finalContent, err := mcpCallTool(context.Background(), ElicitationGatewayURL, sessionID, toolName, map[string]any{"name": "final"}, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(finalStatus).To(Equal(200))
			g.Expect(finalContent).NotTo(BeEmpty())
			g.Expect(finalContent[0].Text).To(ContainSubstring("Hello"))
		}, TestTimeoutMedium, TestRetryInterval).Should(Succeed())
	})

})
