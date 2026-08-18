package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// xraySubscriptionToSingbox converts client-oriented Xray JSON exports into
// one Marten Xray outbound per exported profile. Some subscription panels
// return an array of complete Xray configurations (rather than share links or
// a single sing-box document); keeping each document intact preserves its
// routing, DNS, balancing, and transport behavior.
func xraySubscriptionToSingbox(decoded any) ([]byte, bool, error) {
	type indexedEntry struct {
		index    int
		profile  map[string]interface{}
		outbound map[string]interface{}
	}
	var entries []indexedEntry
	hasXrayProfile := false
	switch value := decoded.(type) {
	case map[string]interface{}:
		if !isXrayConfig(value) {
			return nil, false, nil
		}
		entries = []indexedEntry{{profile: value}}
		hasXrayProfile = true
	case []interface{}:
		if len(value) == 0 {
			return nil, false, nil
		}
		entries = make([]indexedEntry, 0, len(value))
		for index, item := range value {
			profile, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if isXrayConfig(profile) {
				entries = append(entries, indexedEntry{index: index, profile: profile})
				hasXrayProfile = true
				continue
			}
			if strings.TrimSpace(stringValue(profile["type"])) != "" {
				entries = append(entries, indexedEntry{index: index, outbound: profile})
			}
		}
		if !hasXrayProfile {
			return nil, false, nil
		}
	default:
		return nil, false, nil
	}

	outbounds := make([]map[string]interface{}, 0, len(entries))
	usedTags := make(map[string]int, len(entries))
	var lastNormalizeErr error
	for _, candidate := range entries {
		if candidate.outbound != nil {
			outbounds = append(outbounds, candidate.outbound)
			continue
		}
		profile, normalizeErr := cloneAndNormalizeXraySubscriptionProfile(candidate.profile)
		if normalizeErr != nil {
			lastNormalizeErr = fmt.Errorf(
				"[XraySubscriptionParser] normalize item %d: %w",
				candidate.index,
				normalizeErr,
			)
			continue
		}
		remarks := strings.TrimSpace(stringValue(profile["remarks"]))
		delete(profile, "remarks")
		if remarks == "" {
			remarks = firstXrayOutboundTag(profile)
		}
		if remarks == "" {
			remarks = fmt.Sprintf("XRay %d", candidate.index+1)
		}
		tag := uniqueXraySubscriptionTag(remarks, usedTags)
		outbounds = append(outbounds, map[string]interface{}{
			"type":    "xray",
			"tag":     tag,
			"xconfig": profile,
		})
	}
	if len(outbounds) == 0 {
		if lastNormalizeErr != nil {
			return nil, true, lastNormalizeErr
		}
		return nil, true, fmt.Errorf("[XraySubscriptionParser] no usable Xray configs")
	}

	content, err := json.MarshalIndent(map[string]interface{}{"outbounds": outbounds}, "", "  ")
	if err != nil {
		return nil, true, fmt.Errorf("[XraySubscriptionParser] encoding converted config: %w", err)
	}
	return content, true, nil
}

func isXrayConfig(value map[string]interface{}) bool {
	outbounds, ok := value["outbounds"].([]interface{})
	if !ok || len(outbounds) == 0 {
		return false
	}
	for _, item := range outbounds {
		outbound, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if strings.TrimSpace(stringValue(outbound["protocol"])) != "" {
			return true
		}
	}
	return false
}

func firstXrayOutboundTag(profile map[string]interface{}) string {
	outbounds, _ := profile["outbounds"].([]interface{})
	for _, item := range outbounds {
		outbound, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		protocol := strings.ToLower(strings.TrimSpace(stringValue(outbound["protocol"])))
		if protocol == "freedom" || protocol == "blackhole" || protocol == "dns" || protocol == "loopback" {
			continue
		}
		if tag := strings.TrimSpace(stringValue(outbound["tag"])); tag != "" {
			return tag
		}
	}
	return ""
}

func uniqueXraySubscriptionTag(tag string, used map[string]int) string {
	count := used[tag]
	used[tag] = count + 1
	if count == 0 {
		return tag
	}
	return fmt.Sprintf("%s (%d)", tag, count+1)
}

func stringValue(value interface{}) string {
	if value == nil {
		return ""
	}
	text, _ := value.(string)
	return text
}

func cloneAndNormalizeXraySubscriptionProfile(source map[string]interface{}) (map[string]interface{}, error) {
	encoded, err := json.Marshal(source)
	if err != nil {
		return nil, fmt.Errorf("encode Xray config: %w", err)
	}
	var profile map[string]interface{}
	if err := json.Unmarshal(encoded, &profile); err != nil {
		return nil, fmt.Errorf("clone Xray config: %w", err)
	}

	removedInboundTags := xraySubscriptionTags(profile["inbounds"])
	delete(profile, "inbounds")
	rawOutbounds, ok := profile["outbounds"].([]interface{})
	if !ok || len(rawOutbounds) == 0 {
		return nil, fmt.Errorf("Xray config has no outbounds")
	}

	removedOutboundTags := make(map[string]struct{})
	proxyTags := make([]string, 0, len(rawOutbounds))
	outbounds := make([]interface{}, 0, len(rawOutbounds))
	for _, item := range rawOutbounds {
		outbound, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		protocol := strings.ToLower(strings.TrimSpace(stringValue(outbound["protocol"])))
		tag := strings.TrimSpace(stringValue(outbound["tag"]))
		if protocol == "loopback" {
			if tag != "" {
				removedOutboundTags[tag] = struct{}{}
			}
			continue
		}
		outbounds = append(outbounds, outbound)
		if tag != "" && isXraySubscriptionProxyProtocol(protocol) {
			proxyTags = append(proxyTags, tag)
		}
	}
	if len(proxyTags) == 0 {
		return nil, fmt.Errorf("Xray config has no proxy outbound")
	}
	profile["outbounds"] = outbounds

	if routing, ok := profile["routing"].(map[string]interface{}); ok {
		normalizeXraySubscriptionRouting(routing, removedInboundTags, removedOutboundTags, proxyTags)
	}
	return profile, nil
}

func normalizeXraySubscriptionRouting(
	routing map[string]interface{},
	removedInboundTags map[string]struct{},
	removedOutboundTags map[string]struct{},
	proxyTags []string,
) {
	if rawRules, ok := routing["rules"].([]interface{}); ok {
		rules := make([]interface{}, 0, len(rawRules))
		for _, item := range rawRules {
			rule, isMap := item.(map[string]interface{})
			if !isMap {
				rules = append(rules, item)
				continue
			}
			if xraySubscriptionReferences(rule["inboundTag"], removedInboundTags) ||
				xraySubscriptionReferences(rule["outboundTag"], removedOutboundTags) {
				continue
			}
			rules = append(rules, rule)
		}
		routing["rules"] = rules
	}

	proxyTagSet := make(map[string]struct{}, len(proxyTags))
	for _, tag := range proxyTags {
		proxyTagSet[tag] = struct{}{}
	}
	if rawBalancers, ok := routing["balancers"].([]interface{}); ok {
		for _, item := range rawBalancers {
			balancer, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			fallback := strings.TrimSpace(stringValue(balancer["fallbackTag"]))
			if _, usable := proxyTagSet[fallback]; !usable {
				balancer["fallbackTag"] = xraySubscriptionFallback(balancer["selector"], proxyTags)
			}
		}
	}
}

func xraySubscriptionFallback(rawSelectors interface{}, proxyTags []string) string {
	if selectors, ok := rawSelectors.([]interface{}); ok {
		for _, item := range selectors {
			selector := strings.TrimSpace(stringValue(item))
			if selector == "" {
				continue
			}
			for _, tag := range proxyTags {
				if tag == selector || strings.HasPrefix(tag, selector) {
					return tag
				}
			}
		}
	}
	return proxyTags[0]
}

func xraySubscriptionTags(raw interface{}) map[string]struct{} {
	tags := make(map[string]struct{})
	items, ok := raw.([]interface{})
	if !ok {
		return tags
	}
	for _, item := range items {
		if value, ok := item.(map[string]interface{}); ok {
			if tag := strings.TrimSpace(stringValue(value["tag"])); tag != "" {
				tags[tag] = struct{}{}
			}
		}
	}
	return tags
}

func xraySubscriptionReferences(raw interface{}, tags map[string]struct{}) bool {
	if len(tags) == 0 {
		return false
	}
	if text, ok := raw.(string); ok {
		_, found := tags[strings.TrimSpace(text)]
		return found
	}
	items, ok := raw.([]interface{})
	if !ok {
		return false
	}
	for _, item := range items {
		if _, found := tags[strings.TrimSpace(stringValue(item))]; found {
			return true
		}
	}
	return false
}

func isXraySubscriptionProxyProtocol(protocol string) bool {
	switch protocol {
	case "", "freedom", "blackhole", "dns", "loopback":
		return false
	default:
		return true
	}
}
