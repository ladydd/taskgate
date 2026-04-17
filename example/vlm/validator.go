package vlm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/ladydd/taskgate/internal/netsec"
)

// Validator implements service.RequestValidator for VLM requests.
type Validator struct{}

// Validate parses the raw input as a VLM Request and checks all fields.
func (v *Validator) Validate(ctx context.Context, input json.RawMessage) []string {
	var req Request
	if err := json.Unmarshal(input, &req); err != nil {
		return []string{"failed to parse request: " + err.Error()}
	}

	// Use a bounded context for DNS resolution so it doesn't block the request indefinitely.
	dnsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var errs []string

	if req.VLMBaseURL == "" {
		errs = append(errs, "vlm_base_url: must not be empty")
	} else if err := validateHTTPURL(req.VLMBaseURL); err != nil {
		errs = append(errs, "vlm_base_url: "+err.Error())
	} else if err := rejectPrivateURL(dnsCtx, req.VLMBaseURL); err != nil {
		errs = append(errs, "vlm_base_url: "+err.Error())
	}

	if req.VLMAPIKey == "" {
		errs = append(errs, "vlm_api_key: must not be empty")
	}

	if req.VLMModel == "" {
		errs = append(errs, "vlm_model: must not be empty")
	}

	if req.UserText == "" {
		errs = append(errs, "user_text: must not be empty")
	}

	for i, rawURL := range req.ImageURLs {
		if err := validateHTTPURL(rawURL); err != nil {
			errs = append(errs, fmt.Sprintf("image_urls[%d]: %s", i, err.Error()))
		} else if err := rejectPrivateURL(dnsCtx, rawURL); err != nil {
			errs = append(errs, fmt.Sprintf("image_urls[%d]: %s", i, err.Error()))
		}
	}

	return errs
}

func validateHTTPURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL format")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL must use http or https scheme")
	}
	if u.Host == "" {
		return fmt.Errorf("URL is missing a hostname")
	}
	return nil
}

func rejectPrivateURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL format")
	}
	_, err = netsec.ResolvePublicIPs(ctx, u.Hostname(), nil)
	return err
}
