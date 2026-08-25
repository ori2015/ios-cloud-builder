package runner

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	appStoreConnectRoot   = "https://api.appstoreconnect.apple.com"
	betaProcessingTimeout = 40 * time.Minute
	betaPollInterval      = 30 * time.Second
	maxAppStoreResponse   = int64(4 * 1024 * 1024)
	maxBetaGroupConfig    = 64 * 1024
)

var (
	betaGroupIDPattern  = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)
	publicLinkIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`)
	bundleIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{1,254}$`)
	marketingPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

type betaGroupConfig struct {
	GroupID          string `json:"group_id"`
	PublicLinkID     string `json:"public_link_id"`
	SubmitBetaReview bool   `json:"submit_beta_review"`
}

type betaPublishRequest struct {
	BundleID         string
	MarketingVersion string
	BuildNumber      string
	Group            betaGroupConfig
}

type ascResource struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Attributes json.RawMessage `json:"attributes"`
}

type ascListResponse struct {
	Data []ascResource `json:"data"`
}

type ascSingleResponse struct {
	Data *ascResource `json:"data"`
}

type appStoreConnectClient struct {
	baseURL    string
	httpClient *http.Client
	keyID      string
	issuerID   string
	privateKey *ecdsa.PrivateKey
	now        func() time.Time
}

func betaGroupForBundle(raw, bundleID string) (*betaGroupConfig, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if len(raw) > maxBetaGroupConfig || !bundleIDPattern.MatchString(bundleID) {
		return nil, fmt.Errorf("invalid protected beta group configuration")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	groups := make(map[string]betaGroupConfig)
	if err := decoder.Decode(&groups); err != nil || len(groups) == 0 || len(groups) > 100 {
		return nil, fmt.Errorf("invalid protected beta group configuration")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("invalid protected beta group configuration")
	}
	for configuredBundle, group := range groups {
		if !bundleIDPattern.MatchString(configuredBundle) || !betaGroupIDPattern.MatchString(group.GroupID) ||
			!publicLinkIDPattern.MatchString(group.PublicLinkID) {
			return nil, fmt.Errorf("invalid protected beta group configuration")
		}
	}
	group, ok := groups[bundleID]
	if !ok {
		return nil, nil
	}
	return &group, nil
}

func newAppStoreConnectClient(keyID, issuerID, keyValue string) (*appStoreConnectClient, error) {
	keyBytes := []byte(strings.TrimSpace(keyValue))
	if !bytes.Contains(keyBytes, []byte("-----BEGIN PRIVATE KEY-----")) {
		decoded, err := decodeBase64Secret(keyValue)
		if err != nil {
			return nil, fmt.Errorf("decode App Store Connect key for beta publishing")
		}
		keyBytes = decoded
	}
	block, _ := pem.Decode(keyBytes)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("parse App Store Connect key for beta publishing")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	privateKey, ok := parsed.(*ecdsa.PrivateKey)
	if err != nil || !ok || privateKey.Curve != elliptic.P256() {
		return nil, fmt.Errorf("parse App Store Connect key for beta publishing")
	}
	return &appStoreConnectClient{
		baseURL: appStoreConnectRoot, httpClient: &http.Client{Timeout: 60 * time.Second},
		keyID: keyID, issuerID: issuerID, privateKey: privateKey, now: time.Now,
	}, nil
}

func (c *appStoreConnectClient) token() (string, error) {
	now := c.now().Unix()
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": c.keyID, "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"iss": c.issuerID, "iat": now, "exp": now + 900, "aud": "appstoreconnect-v1",
	})
	encode := base64.RawURLEncoding.EncodeToString
	unsigned := encode(header) + "." + encode(payload)
	r, s, err := ecdsa.Sign(rand.Reader, c.privateKey, hashString(unsigned))
	if err != nil {
		return "", fmt.Errorf("sign App Store Connect request")
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return unsigned + "." + encode(signature), nil
}

func hashString(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}

func (c *appStoreConnectClient) request(ctx context.Context, method, apiPath string, query url.Values, body any, output any) error {
	requestURL := c.baseURL + apiPath
	if len(query) != 0 {
		requestURL += "?" + query.Encode()
	}
	var bodyReader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("prepare App Store Connect request")
		}
		bodyReader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, bodyReader)
	if err != nil {
		return fmt.Errorf("prepare App Store Connect request")
	}
	token, err := c.token()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("contact App Store Connect: %w", err)
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxAppStoreResponse+1))
	if err != nil || int64(len(contents)) > maxAppStoreResponse {
		return fmt.Errorf("read App Store Connect response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return appStoreConnectHTTPError(response.StatusCode, contents)
	}
	if output != nil && len(contents) != 0 {
		if err := json.Unmarshal(contents, output); err != nil {
			return fmt.Errorf("parse App Store Connect response")
		}
	}
	return nil
}

func appStoreConnectHTTPError(status int, contents []byte) error {
	var envelope struct {
		Errors []struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(contents, &envelope)
	var details []string
	for _, item := range envelope.Errors {
		detail := strings.TrimSpace(item.Title + ": " + item.Detail)
		if len(detail) > 500 {
			detail = detail[:500]
		}
		if detail != ":" {
			details = append(details, detail)
		}
	}
	if len(details) == 0 {
		return fmt.Errorf("App Store Connect request failed with HTTP %d", status)
	}
	return fmt.Errorf("App Store Connect request failed with HTTP %d: %s", status, strings.Join(details, " | "))
}

func publishToBetaGroup(ctx context.Context, api *appStoreConnectClient, request betaPublishRequest, privateLog io.Writer) error {
	if !bundleIDPattern.MatchString(request.BundleID) || !marketingPattern.MatchString(request.MarketingVersion) ||
		!buildPattern.MatchString(request.BuildNumber) {
		return fmt.Errorf("invalid TestFlight publishing metadata")
	}
	appID, err := findASCApp(ctx, api, request.BundleID)
	if err != nil {
		return err
	}
	group, err := findASCBetaGroup(ctx, api, appID, request.Group)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(privateLog, "Waiting for TestFlight build %s (%s) to finish processing.\n", request.MarketingVersion, request.BuildNumber)
	build, err := waitForASCBuild(ctx, api, appID, request.MarketingVersion, request.BuildNumber, privateLog)
	if err != nil {
		return err
	}
	if build.UsesNonExemptEncryption == nil {
		if err := setASCExportCompliance(ctx, api, build.ID); err != nil {
			return err
		}
	}
	if err := setASCWhatsNew(ctx, api, build.ID,
		fmt.Sprintf("Automated central build %s (%s).", request.MarketingVersion, request.BuildNumber)); err != nil {
		return err
	}
	if err := attachASCBuild(ctx, api, group.ID, build.ID); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(privateLog, "Build attached to the configured TestFlight beta group.")
	if !group.Internal && request.Group.SubmitBetaReview {
		blockers, err := ascExternalTestingBlockers(ctx, api, appID)
		if err != nil {
			return err
		}
		for _, blocker := range blockers {
			_, _ = fmt.Fprintf(privateLog, "External testing prerequisite: %s\n", blocker)
		}
		if len(blockers) == 0 {
			if err := submitASCBetaReview(ctx, api, build.ID); err != nil {
				return err
			}
		}
	}
	state, err := ascExternalBuildState(ctx, api, build.ID)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(privateLog, "TestFlight external build state: %s.\n", state)
	after, err := getASCBetaGroup(ctx, api, group.ID)
	if err != nil {
		return err
	}
	if group.PublicLink != "" && after.PublicLink != group.PublicLink {
		return fmt.Errorf("configured TestFlight beta group public link changed during publishing")
	}
	return nil
}

type ascBuild struct {
	ID                      string
	ProcessingState         string
	UsesNonExemptEncryption *bool
}

type ascBetaGroup struct {
	ID           string
	Name         string
	PublicLinkID string
	PublicLink   string
	Internal     bool
}

func findASCApp(ctx context.Context, api *appStoreConnectClient, bundleID string) (string, error) {
	query := url.Values{"filter[bundleId]": {bundleID}, "limit": {"200"}}
	var response ascListResponse
	if err := api.request(ctx, http.MethodGet, "/v1/apps", query, nil, &response); err != nil {
		return "", err
	}
	for _, app := range response.Data {
		var attributes struct {
			BundleID string `json:"bundleId"`
		}
		if json.Unmarshal(app.Attributes, &attributes) == nil && attributes.BundleID == bundleID {
			return app.ID, nil
		}
	}
	return "", fmt.Errorf("App Store Connect has no exact app for the uploaded bundle identifier")
}

func findASCBetaGroup(ctx context.Context, api *appStoreConnectClient, appID string, configured betaGroupConfig) (ascBetaGroup, error) {
	var response ascListResponse
	if err := api.request(ctx, http.MethodGet, "/v1/apps/"+url.PathEscape(appID)+"/betaGroups", url.Values{"limit": {"200"}}, nil, &response); err != nil {
		return ascBetaGroup{}, err
	}
	for _, resource := range response.Data {
		if resource.ID != configured.GroupID {
			continue
		}
		group, err := decodeASCBetaGroup(resource)
		if err != nil || group.PublicLinkID != configured.PublicLinkID ||
			!strings.HasSuffix(strings.TrimRight(group.PublicLink, "/"), "/"+configured.PublicLinkID) {
			return ascBetaGroup{}, fmt.Errorf("configured TestFlight beta group does not own the expected public link")
		}
		return group, nil
	}
	return ascBetaGroup{}, fmt.Errorf("configured TestFlight beta group does not belong to the uploaded app")
}

func getASCBetaGroup(ctx context.Context, api *appStoreConnectClient, groupID string) (ascBetaGroup, error) {
	var response ascSingleResponse
	if err := api.request(ctx, http.MethodGet, "/v1/betaGroups/"+url.PathEscape(groupID), nil, nil, &response); err != nil {
		return ascBetaGroup{}, err
	}
	if response.Data == nil {
		return ascBetaGroup{}, fmt.Errorf("configured TestFlight beta group disappeared")
	}
	return decodeASCBetaGroup(*response.Data)
}

func decodeASCBetaGroup(resource ascResource) (ascBetaGroup, error) {
	var attributes struct {
		Name            string `json:"name"`
		PublicLinkID    string `json:"publicLinkId"`
		PublicLink      string `json:"publicLink"`
		IsInternalGroup bool   `json:"isInternalGroup"`
	}
	if err := json.Unmarshal(resource.Attributes, &attributes); err != nil {
		return ascBetaGroup{}, fmt.Errorf("parse TestFlight beta group")
	}
	return ascBetaGroup{ID: resource.ID, Name: attributes.Name, PublicLinkID: attributes.PublicLinkID,
		PublicLink: attributes.PublicLink, Internal: attributes.IsInternalGroup}, nil
}

func waitForASCBuild(ctx context.Context, api *appStoreConnectClient, appID, version, buildNumber string, privateLog io.Writer) (ascBuild, error) {
	deadline := time.Now().Add(betaProcessingTimeout)
	for attempt := 1; ; attempt++ {
		build, found, err := findASCBuild(ctx, api, appID, version, buildNumber)
		if err != nil {
			return ascBuild{}, err
		}
		if found {
			switch build.ProcessingState {
			case "VALID":
				_, _ = fmt.Fprintln(privateLog, "App Store Connect finished processing the exact uploaded build.")
				return build, nil
			case "INVALID", "FAILED":
				return ascBuild{}, fmt.Errorf("App Store Connect processed the uploaded build as %s", build.ProcessingState)
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ascBuild{}, fmt.Errorf("timed out waiting for the exact uploaded build to finish App Store Connect processing")
		}
		_, _ = fmt.Fprintf(privateLog, "TestFlight processing poll %d; retrying in %s.\n", attempt, betaPollInterval)
		timer := time.NewTimer(min(betaPollInterval, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ascBuild{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func findASCBuild(ctx context.Context, api *appStoreConnectClient, appID, version, buildNumber string) (ascBuild, bool, error) {
	query := url.Values{
		"filter[app]": {appID}, "filter[version]": {buildNumber},
		"filter[preReleaseVersion.version]": {version}, "limit": {"200"},
	}
	var response ascListResponse
	if err := api.request(ctx, http.MethodGet, "/v1/builds", query, nil, &response); err != nil {
		return ascBuild{}, false, err
	}
	if len(response.Data) == 0 {
		return ascBuild{}, false, nil
	}
	if len(response.Data) != 1 {
		return ascBuild{}, false, fmt.Errorf("App Store Connect returned multiple builds for the exact version and build number")
	}
	var attributes struct {
		ProcessingState         string `json:"processingState"`
		UsesNonExemptEncryption *bool  `json:"usesNonExemptEncryption"`
	}
	if err := json.Unmarshal(response.Data[0].Attributes, &attributes); err != nil {
		return ascBuild{}, false, fmt.Errorf("parse App Store Connect build")
	}
	return ascBuild{ID: response.Data[0].ID, ProcessingState: attributes.ProcessingState,
		UsesNonExemptEncryption: attributes.UsesNonExemptEncryption}, true, nil
}

func setASCExportCompliance(ctx context.Context, api *appStoreConnectClient, buildID string) error {
	body := map[string]any{"data": map[string]any{"type": "builds", "id": buildID,
		"attributes": map[string]any{"usesNonExemptEncryption": false}}}
	return api.request(ctx, http.MethodPatch, "/v1/builds/"+url.PathEscape(buildID), nil, body, nil)
}

func setASCWhatsNew(ctx context.Context, api *appStoreConnectClient, buildID, message string) error {
	apiPath := "/v1/builds/" + url.PathEscape(buildID) + "/betaBuildLocalizations"
	var response ascListResponse
	if err := api.request(ctx, http.MethodGet, apiPath, url.Values{"limit": {"200"}}, nil, &response); err != nil {
		return err
	}
	if len(response.Data) == 0 {
		body := map[string]any{"data": map[string]any{"type": "betaBuildLocalizations",
			"attributes":    map[string]any{"locale": "en-US", "whatsNew": message},
			"relationships": map[string]any{"build": map[string]any{"data": map[string]string{"type": "builds", "id": buildID}}}}}
		return api.request(ctx, http.MethodPost, "/v1/betaBuildLocalizations", nil, body, nil)
	}
	for _, localization := range response.Data {
		body := map[string]any{"data": map[string]any{"type": "betaBuildLocalizations", "id": localization.ID,
			"attributes": map[string]any{"whatsNew": message}}}
		if err := api.request(ctx, http.MethodPatch, "/v1/betaBuildLocalizations/"+url.PathEscape(localization.ID), nil, body, nil); err != nil {
			return err
		}
	}
	return nil
}

func attachASCBuild(ctx context.Context, api *appStoreConnectClient, groupID, buildID string) error {
	body := map[string]any{"data": []map[string]string{{"type": "builds", "id": buildID}}}
	err := api.request(ctx, http.MethodPost, "/v1/betaGroups/"+url.PathEscape(groupID)+"/relationships/builds", nil, body, nil)
	if err == nil {
		return nil
	}
	present, checkErr := ascBuildInGroup(ctx, api, groupID, buildID)
	if checkErr == nil && present {
		return nil
	}
	return err
}

func ascBuildInGroup(ctx context.Context, api *appStoreConnectClient, groupID, buildID string) (bool, error) {
	var response ascListResponse
	if err := api.request(ctx, http.MethodGet, "/v1/betaGroups/"+url.PathEscape(groupID)+"/relationships/builds",
		url.Values{"limit": {"200"}}, nil, &response); err != nil {
		return false, err
	}
	for _, build := range response.Data {
		if build.ID == buildID {
			return true, nil
		}
	}
	return false, nil
}

func ascExternalTestingBlockers(ctx context.Context, api *appStoreConnectClient, appID string) ([]string, error) {
	var blockers []string
	var localizations ascListResponse
	if err := api.request(ctx, http.MethodGet, "/v1/apps/"+url.PathEscape(appID)+"/betaAppLocalizations",
		url.Values{"limit": {"200"}}, nil, &localizations); err != nil {
		return nil, err
	}
	if len(localizations.Data) == 0 {
		blockers = append(blockers, "beta app description and feedback email are missing")
	} else {
		for _, localization := range localizations.Data {
			var attributes struct {
				Description   string `json:"description"`
				FeedbackEmail string `json:"feedbackEmail"`
			}
			if json.Unmarshal(localization.Attributes, &attributes) != nil ||
				attributes.Description == "" || attributes.FeedbackEmail == "" {
				blockers = append(blockers, "beta app localization is incomplete")
				break
			}
		}
	}
	var detail ascSingleResponse
	if err := api.request(ctx, http.MethodGet, "/v1/apps/"+url.PathEscape(appID)+"/betaAppReviewDetail", nil, nil, &detail); err != nil {
		return nil, err
	}
	if detail.Data == nil {
		blockers = append(blockers, "Beta App Review contact information is missing")
	} else {
		var attributes struct {
			FirstName string `json:"contactFirstName"`
			LastName  string `json:"contactLastName"`
			Phone     string `json:"contactPhone"`
			Email     string `json:"contactEmail"`
		}
		if json.Unmarshal(detail.Data.Attributes, &attributes) != nil || attributes.FirstName == "" ||
			attributes.LastName == "" || attributes.Phone == "" || attributes.Email == "" {
			blockers = append(blockers, "Beta App Review contact information is incomplete")
		}
	}
	return blockers, nil
}

func submitASCBetaReview(ctx context.Context, api *appStoreConnectClient, buildID string) error {
	var existing ascSingleResponse
	if err := api.request(ctx, http.MethodGet, "/v1/builds/"+url.PathEscape(buildID)+"/betaAppReviewSubmission", nil, nil, &existing); err != nil {
		return err
	}
	if existing.Data != nil {
		return nil
	}
	body := map[string]any{"data": map[string]any{"type": "betaAppReviewSubmissions",
		"relationships": map[string]any{"build": map[string]any{"data": map[string]string{"type": "builds", "id": buildID}}}}}
	return api.request(ctx, http.MethodPost, "/v1/betaAppReviewSubmissions", nil, body, nil)
}

func ascExternalBuildState(ctx context.Context, api *appStoreConnectClient, buildID string) (string, error) {
	var response ascSingleResponse
	if err := api.request(ctx, http.MethodGet, "/v1/builds/"+url.PathEscape(buildID)+"/buildBetaDetail", nil, nil, &response); err != nil {
		return "", err
	}
	if response.Data == nil {
		return "UNKNOWN", nil
	}
	var attributes struct {
		ExternalBuildState string `json:"externalBuildState"`
	}
	if err := json.Unmarshal(response.Data.Attributes, &attributes); err != nil {
		return "", fmt.Errorf("parse TestFlight external build state")
	}
	return attributes.ExternalBuildState, nil
}
