package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"mcpx/internal/config"
	"mcpx/internal/guidancebinding"
)

type resolvedPluginGuidance struct {
	GuidanceID     string
	ProviderPlugin string
	Summary        string
	Scope          string
	Revision       string
	Content        string
}

func (r *Runtime) resolvePluginGuidance(wsPath, scope string) ([]resolvedPluginGuidance, error) {
	scope = strings.TrimSpace(scope)
	if scope != config.PluginGuidanceScopeAttachment && scope != config.PluginGuidanceScopeContext {
		return nil, fmt.Errorf("unsupported Guidance scope %q", scope)
	}
	names, err := r.activePluginNames(wsPath)
	if err != nil {
		return nil, err
	}
	items := []resolvedPluginGuidance{}
	seen := map[string]string{}
	for _, name := range names {
		server, active, err := r.effectivePluginForWorkspace(wsPath, name)
		if err != nil {
			return nil, err
		}
		if !active || server.Plugin == nil {
			continue
		}
		for _, declared := range server.Plugin.Guidance {
			if strings.TrimSpace(declared.Scope) != scope {
				continue
			}
			id := strings.TrimSpace(declared.ID)
			if prior := seen[id]; prior != "" && prior != name {
				return nil, fmt.Errorf("Plugin Guidance %q is ambiguous between %q and %q", id, prior, name)
			}
			seen[id] = name
			content, revision, err := readPluginGuidanceAsset(declared.Path, declared.FrontMatter)
			if err != nil {
				return nil, fmt.Errorf("Plugin %q Guidance %q: %w", name, id, err)
			}
			items = append(items, resolvedPluginGuidance{
				GuidanceID: id, ProviderPlugin: name, Summary: strings.TrimSpace(declared.Summary),
				Scope: scope, Revision: revision, Content: content,
			})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].GuidanceID != items[j].GuidanceID {
			return items[i].GuidanceID < items[j].GuidanceID
		}
		return items[i].ProviderPlugin < items[j].ProviderPlugin
	})
	return items, nil
}

func readPluginGuidanceAsset(path string, stripFrontMatter ...bool) (string, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", fmt.Errorf("path is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("path is a directory: %s", path)
	}
	if info.Size() > config.PluginGuidanceHardMaxBytes {
		return "", "", fmt.Errorf("asset is %d bytes; maximum is %d", info.Size(), config.PluginGuidanceHardMaxBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	if len(data) > config.PluginGuidanceHardMaxBytes {
		return "", "", fmt.Errorf("asset is %d bytes; maximum is %d", len(data), config.PluginGuidanceHardMaxBytes)
	}
	if !utf8.Valid(data) {
		return "", "", fmt.Errorf("asset must be valid UTF-8")
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", "", fmt.Errorf("asset is empty")
	}
	digest := sha256.Sum256(data)
	content := string(data)
	strip := len(stripFrontMatter) > 0 && stripFrontMatter[0]
	if strip {
		normalized := strings.ReplaceAll(content, "\r\n", "\n")
		if !strings.HasPrefix(normalized, "---\n") {
			return "", "", fmt.Errorf("Guidance front matter is missing or malformed")
		}
		rest := strings.TrimPrefix(normalized, "---\n")
		boundary := strings.Index(rest, "\n---\n")
		if boundary < 0 {
			return "", "", fmt.Errorf("Guidance front matter is missing or malformed")
		}
		content = strings.TrimSpace(rest[boundary+5:])
		if content == "" {
			return "", "", fmt.Errorf("Guidance body is empty")
		}
	}
	return content, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func pluginGuidanceSetRevision(items []resolvedPluginGuidance) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, strings.Join([]string{item.GuidanceID, item.ProviderPlugin, item.Scope, item.Revision}, "\x00"))
	}
	sort.Strings(parts)
	digest := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func guidanceMetadataMap(item resolvedPluginGuidance) map[string]any {
	result := map[string]any{
		"guidance_id": item.GuidanceID, "provider_plugin": item.ProviderPlugin,
		"scope": item.Scope, "revision": item.Revision,
	}
	if item.Summary != "" {
		result["summary"] = item.Summary
	}
	return result
}

func guidanceDeliveryMap(delivery guidancebinding.Delivery, currentRevision string) map[string]any {
	result := map[string]any{
		"guidance_id": delivery.Binding.GuidanceID, "provider_plugin": delivery.Binding.ProviderPlugin,
		"scope": delivery.Binding.Scope, "revision": delivery.Binding.Revision,
		"current_revision": currentRevision, "consumer_id": delivery.Binding.ConsumerID,
		"bound_at": delivery.Binding.BoundAt, "delivered": delivery.Delivered,
	}
	if delivery.Delivered {
		result["content"] = delivery.Content
	}
	return result
}

func (r *Runtime) contextGuidanceByID(wsPath, guidanceID string) (resolvedPluginGuidance, error) {
	guidanceID = strings.TrimSpace(guidanceID)
	items, err := r.resolvePluginGuidance(wsPath, config.PluginGuidanceScopeContext)
	if err != nil {
		return resolvedPluginGuidance{}, err
	}
	for _, item := range items {
		if item.GuidanceID == guidanceID {
			return item, nil
		}
	}
	return resolvedPluginGuidance{}, fmt.Errorf("context Guidance %q is not available in the effective Plugin graph", guidanceID)
}

func (r *Runtime) bindContextGuidance(ctx context.Context, remoteSessionID, wsPath, guidanceID, consumerID, deliveryKey, providerConstraint string) (map[string]any, error) {
	item, err := r.contextGuidanceByID(wsPath, guidanceID)
	if err != nil {
		return nil, err
	}
	providerConstraint = strings.TrimSpace(providerConstraint)
	if providerConstraint != "" && item.ProviderPlugin != providerConstraint {
		return nil, fmt.Errorf("Plugin %q cannot bind Guidance %q provided by %q", providerConstraint, item.GuidanceID, item.ProviderPlugin)
	}
	delivery, err := r.guidanceBindings.Bind(ctx, remoteSessionID, consumerID, deliveryKey, guidancebinding.Candidate{
		GuidanceID: item.GuidanceID, ProviderPlugin: item.ProviderPlugin, Scope: item.Scope,
		Revision: item.Revision, Content: item.Content,
	})
	if err != nil {
		return nil, err
	}
	return guidanceDeliveryMap(delivery, item.Revision), nil
}

func (r *Runtime) bindAttachmentGuidance(ctx context.Context, remoteSessionID, wsPath, attachmentID, deliveryKey string) (map[string]any, string, error) {
	items, err := r.resolvePluginGuidance(wsPath, config.PluginGuidanceScopeAttachment)
	if err != nil {
		return nil, "", err
	}
	currentRevision := pluginGuidanceSetRevision(items)
	bindings := make([]map[string]any, 0, len(items))
	for _, item := range items {
		delivery, err := r.guidanceBindings.Bind(ctx, remoteSessionID, attachmentID, deliveryKey, guidancebinding.Candidate{
			GuidanceID: item.GuidanceID, ProviderPlugin: item.ProviderPlugin, Scope: item.Scope,
			Revision: item.Revision, Content: item.Content,
		})
		if err != nil {
			return nil, "", err
		}
		bindings = append(bindings, guidanceDeliveryMap(delivery, item.Revision))
	}
	return map[string]any{
		"scope": config.PluginGuidanceScopeAttachment, "consumer_id": attachmentID,
		"current_revision": currentRevision, "bindings": bindings,
	}, currentRevision, nil
}
