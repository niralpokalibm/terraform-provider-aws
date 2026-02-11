// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package logs

import (
	"context"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	awstypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-provider-aws/internal/conns"
	"github.com/hashicorp/terraform-provider-aws/internal/errs"
	"github.com/hashicorp/terraform-provider-aws/internal/vcr"
)

var (
	// Rate limiter for DescribeLogGroups API (5 TPS limit)
	// Using 1 TPS (1000ms) to be extremely conservative due to SDK retries
	describeLogGroupsRateLimiterMu   sync.Mutex
	describeLogGroupsLastCallTime    time.Time
	describeLogGroupsRateLimitDelay  = 1000 * time.Millisecond // 1 TPS (very conservative)
	
	// Rate limiter for ListTagsForResource API (15 TPS documented limit appears to be burst only)
	// Using 1000ms (1 TPS) - extremely conservative but avoids throttling
	listTagsRateLimiterMu   sync.Mutex
	listTagsLastCallTime    time.Time
	listTagsRateLimitDelay  = 1000 * time.Millisecond // 1 TPS (very conservative)
	
	// Rate limiter for Resource Groups Tagging API GetResources (15 TPS limit)
	// Using 200ms (5 TPS) to be conservative
	getResourcesRateLimiterMu   sync.Mutex
	getResourcesLastCallTime    time.Time
	getResourcesRateLimitDelay  = 200 * time.Millisecond // 5 TPS
)

// waitForDescribeLogGroupsRateLimit enforces rate limiting for DescribeLogGroups API (5 TPS)
func waitForDescribeLogGroupsRateLimit(ctx context.Context) error {
	return waitForRateLimit(ctx, &describeLogGroupsRateLimiterMu, &describeLogGroupsLastCallTime, describeLogGroupsRateLimitDelay, "DescribeLogGroups")
}

// waitForListTagsRateLimit enforces rate limiting for ListTagsForResource API (15 TPS)
func waitForListTagsRateLimit(ctx context.Context) error {
	return waitForRateLimit(ctx, &listTagsRateLimiterMu, &listTagsLastCallTime, listTagsRateLimitDelay, "ListTagsForResource")
}

// waitForGetResourcesRateLimit enforces rate limiting for Resource Groups Tagging API GetResources (15 TPS)
func waitForGetResourcesRateLimit(ctx context.Context) error {
	return waitForRateLimit(ctx, &getResourcesRateLimiterMu, &getResourcesLastCallTime, getResourcesRateLimitDelay, "GetResources")
}

// waitForRateLimit is a generic rate limiter implementation
func waitForRateLimit(ctx context.Context, mu *sync.Mutex, lastCallTime *time.Time, delay time.Duration, operation string) error {
	mu.Lock()
	defer mu.Unlock()
	
	now := time.Now()
	timeSinceLastCall := now.Sub(*lastCallTime)
	
	if timeSinceLastCall < delay {
		sleepTime := delay - timeSinceLastCall
		tflog.Debug(ctx, "Rate limiting: sleeping to avoid throttling", map[string]interface{}{
			"operation":          operation,
			"sleep_ms":           sleepTime.Milliseconds(),
			"time_since_last_ms": timeSinceLastCall.Milliseconds(),
		})
		
		select {
		case <-time.After(sleepTime):
			// Sleep completed
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		tflog.Debug(ctx, "Rate limiter: no sleep needed", map[string]interface{}{
			"operation":          operation,
			"time_since_last_ms": timeSinceLastCall.Milliseconds(),
		})
	}
	
	*lastCallTime = time.Now()
	tflog.Debug(ctx, "Rate limiter: proceeding with API call", map[string]interface{}{
		"operation": operation,
	})
	return nil
}

func (p *servicePackage) withExtraOptions(ctx context.Context, config map[string]any) []func(*cloudwatchlogs.Options) {
	cfg := *(config["aws_sdkv2_config"].(*aws.Config))

	return []func(*cloudwatchlogs.Options){
		func(o *cloudwatchlogs.Options) {
			retryables := []retry.IsErrorRetryable{
				retry.IsErrorRetryableFunc(func(err error) aws.Ternary {
					if errs.IsAErrorMessageContains[*awstypes.LimitExceededException](err, "Resource limit exceeded") {
						return aws.FalseTernary
					}
					// Disable retries for throttling errors - we handle rate limiting ourselves
					if errs.IsAErrorMessageContains[*awstypes.ThrottlingException](err, "Rate exceeded") {
						tflog.Debug(ctx, "Throttling detected - not retrying (rate limiter will handle)", map[string]interface{}{
							"error": err.Error(),
						})
						return aws.FalseTernary
					}
					return aws.UnknownTernary // Delegate to configured Retryer.
				}),
			}
			// Include go-vcr retryable to prevent generated client retryer from being overridden
			if inContext, ok := conns.FromContext(ctx); ok && inContext.VCREnabled() {
				tflog.Info(ctx, "overriding retry behavior to immediately return VCR errors")
				retryables = append(retryables, vcr.InteractionNotFoundRetryableFunc)
			}

			o.Retryer = conns.AddIsErrorRetryables(cfg.Retryer().(aws.RetryerV2), retryables...)
		},
	}
}
