package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"

	"github.com/fengwk/cliproxyapi-commandcode-provider/internal/catalogdata"
)

// cliModel is one reviewed CLI 1.79.2 catalog entry. Every capability field is
// optional because the source catalog omits unknown facts rather than encoding
// a default.
type cliModel struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	InputModalities  []string `json:"inputModalities"`
	ReasoningEfforts []string `json:"reasoningEfforts"`
	ContextWindow    int64    `json:"contextWindow"`
	MaxOutputTokens  int64    `json:"maxOutputTokens"`
	Reasoning        *bool    `json:"reasoning"`
}

// publicModel is one public GET /provider/v1/models entry. Data is a pointer so
// a missing or null "data" field is distinguishable from an explicit empty list.
type publicModel struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Created            int64    `json:"created"`
	OwnedBy            string   `json:"owned_by"`
	ContextLength      int64    `json:"context_length"`
	SupportedEndpoints []string `json:"supported_endpoints"`
}

type publicCatalog struct {
	Data *[]publicModel `json:"data"`
}

type cliCatalog struct {
	Models []cliModel `json:"models"`
}

// catalogIndex is the parsed, reviewed compatibility snapshot embedded at build
// time: the CLI facts and the public provider catalog bound to one CLI version.
type catalogIndex struct {
	cli         map[string]cliModel
	cliOrder    []string
	public      map[string]publicModel
	publicOrder []string
}

var (
	embeddedOnce  sync.Once
	embeddedCache catalogIndex
)

// loadEmbeddedIndex parses the embedded reviewed snapshot exactly once. A
// malformed embedded bundle yields an empty index; the catalog is then simply
// absent rather than invented.
func loadEmbeddedIndex() catalogIndex {
	embeddedOnce.Do(func() {
		index := catalogIndex{cli: map[string]cliModel{}, public: map[string]publicModel{}}
		var cli cliCatalog
		if err := json.Unmarshal(catalogdata.CLIJSON, &cli); err == nil {
			for _, model := range cli.Models {
				id := nativeModelID(model.ID)
				if id == "" {
					continue
				}
				if _, ok := index.cli[id]; ok {
					continue
				}
				index.cli[id] = model
				index.cliOrder = append(index.cliOrder, id)
			}
		}
		var public publicCatalog
		if err := json.Unmarshal(catalogdata.ProviderJSON, &public); err == nil && public.Data != nil {
			for _, model := range *public.Data {
				id := nativeModelID(model.ID)
				if id == "" {
					continue
				}
				if _, ok := index.public[id]; ok {
					continue
				}
				index.public[id] = model
				index.publicOrder = append(index.publicOrder, id)
			}
		}
		embeddedCache = index
	})
	return embeddedCache
}

// catalogCLIVersion is the reviewed CLI compatibility revision bound to the
// embedded snapshot. It is read-only and never overridden by a freshly seen
// upstream version.
func catalogCLIVersion() string {
	return catalogdata.CLIVersion
}

// nativeModelID strips the fixed public namespace from a requested model id.
func nativeModelID(model string) string {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(model, modelPrefix) {
		return strings.TrimPrefix(model, modelPrefix)
	}
	return model
}

// modelFacts is the union of reviewed CLI metadata and live public catalog
// facts for one native model. Unknown fields stay zero/absent.
type modelFacts struct {
	native        string
	name          string
	created       int64
	contextWindow int64
	maxOutput     int64
	modalities    []string
	efforts       []string
	reasoning     bool
	reasoningSeen bool
	endpoints     []string
	userDefined   bool
}

// factsFromCLI builds facts from a reviewed CLI entry.
func factsFromCLI(model cliModel) modelFacts {
	facts := modelFacts{
		native:        nativeModelID(model.ID),
		name:          model.Name,
		contextWindow: model.ContextWindow,
		maxOutput:     model.MaxOutputTokens,
		modalities:    model.InputModalities,
		efforts:       model.ReasoningEfforts,
	}
	if model.Reasoning != nil {
		facts.reasoningSeen = true
		facts.reasoning = *model.Reasoning
	}
	return facts
}

// mergePublic supplements facts with live public catalog values. Endpoints and
// created always come from the public entry; context/modalities/reasoning are
// only filled when the public entry leaves them unknown.
func (f modelFacts) mergePublic(model publicModel, haveCLI bool) modelFacts {
	f.created = model.Created
	f.endpoints = model.SupportedEndpoints
	if name := strings.TrimSpace(model.Name); name != "" {
		f.name = name
	}
	if !haveCLI || f.contextWindow == 0 {
		if model.ContextLength > 0 {
			f.contextWindow = model.ContextLength
		}
	}
	return f
}

// applyManual overlays explicit manual-model metadata, then marks the model
// user-defined. Only supplied fields override discovered facts.
func (f modelFacts) applyManual(manual ManualModel) modelFacts {
	f.userDefined = true
	if manual.ContextWindow > 0 {
		f.contextWindow = manual.ContextWindow
	}
	if manual.MaxOutputTokens > 0 {
		f.maxOutput = manual.MaxOutputTokens
	}
	if len(manual.InputModalities) > 0 {
		f.modalities = manual.InputModalities
	}
	if len(manual.ReasoningEfforts) > 0 {
		f.efforts = manual.ReasoningEfforts
		f.reasoningSeen = true
		f.reasoning = true
	}
	if strings.TrimSpace(manual.Protocol) != "" {
		if format, err := parseProtocol(manual.Protocol); err == nil {
			if endpoint := endpointForFormat(format); endpoint != "" {
				f.endpoints = []string{endpoint}
			}
		}
	}
	return f
}

// visionEnabled reports whether a model accepts inline images.
func (f modelFacts) visionEnabled() bool {
	for _, modality := range f.modalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return true
		}
	}
	return false
}

// thinkingSupport publishes only the reasoning facts that were actually
// observed. It never invents budgets, zero-allowed flags or effort levels.
func (f modelFacts) thinkingSupport() *pluginapi.ThinkingSupport {
	if len(f.efforts) > 0 {
		return &pluginapi.ThinkingSupport{Levels: append([]string(nil), f.efforts...)}
	}
	if f.reasoningSeen && f.reasoning {
		return &pluginapi.ThinkingSupport{}
	}
	return nil
}

// modelInfo renders one published ModelInfo. The public namespace is applied
// exactly once and unknown facts remain zero.
func (f modelFacts) modelInfo() pluginapi.ModelInfo {
	ownedBy := "command-code"
	return pluginapi.ModelInfo{
		ID:                        modelPrefix + f.native,
		Object:                    "model",
		Created:                   f.created,
		OwnedBy:                   ownedBy,
		DisplayName:               f.name,
		Name:                      f.native,
		ContextLength:             f.contextWindow,
		MaxCompletionTokens:       f.maxOutput,
		SupportedInputModalities:  append([]string(nil), f.modalities...),
		SupportedOutputModalities: []string{"text"},
		Thinking:                  f.thinkingSupport(),
		UserDefined:               f.userDefined,
	}
}

// catalogInfo is the read-only catalog status shown by management clients.
type catalogInfo struct {
	Count     int    `json:"count"`
	Source    string `json:"source"`
	FetchedAt string `json:"fetched_at"`
	Stale     bool   `json:"stale"`
}

// catalogState caches the last successful live public catalog. It is bounded to
// one snapshot and never grows, and it is bound to the authority and config
// generation it was fetched under so a change of authority cannot reuse a stale
// snapshot or let an old in-flight refresh overwrite the new one.
type catalogState struct {
	mu          sync.Mutex
	live        []publicModel
	liveAt      time.Time
	liveBase    string
	liveGen     uint64
	lastAttempt time.Time
	lastFailed  bool
	refreshing  bool
}

const (
	// refreshInterval bounds how often a discovery-driven live refresh may run.
	refreshInterval = time.Minute
	// catalogStaleAfter is the age beyond which a live snapshot is reported grey.
	catalogStaleAfter = 30 * time.Minute
	// catalogHTTPDeadline bounds the catalog probe HTTP call.
	catalogHTTPDeadline = 15 * time.Second
	// maxCatalogEntries bounds one decoded public catalog so a hostile response
	// cannot expand the published set without bound.
	maxCatalogEntries = 5000
)

// snapshot returns the current public catalog: the last successful live fetch
// when it is still bound to cfg's authority and config generation, otherwise
// the embedded reviewed snapshot marked stale. A snapshot whose last refresh
// failed or that has exceeded catalogStaleAfter is reported stale, but the
// last-good data is never cleared.
func (m *Manager) catalogSnapshot(cfg Config) ([]publicModel, catalogInfo) {
	index := loadEmbeddedIndex()
	generation := m.configGeneration()
	m.catalog.mu.Lock()
	defer m.catalog.mu.Unlock()
	if m.catalog.live != nil && m.catalog.liveBase == cfg.BaseURL && m.catalog.liveGen == generation {
		stale := m.catalog.lastFailed || time.Since(m.catalog.liveAt) > catalogStaleAfter
		return m.catalog.live, catalogInfo{
			Count:     len(m.catalog.live),
			Source:    "live",
			FetchedAt: m.catalog.liveAt.UTC().Format(time.RFC3339),
			Stale:     stale,
		}
	}
	return embeddedPublicModels(index), catalogInfo{
		Count:     len(index.publicOrder),
		Source:    "embedded",
		FetchedAt: catalogdata.SnapshotDate,
		Stale:     true,
	}
}

// embeddedPublicModels materializes the embedded public catalog in reviewed order.
func embeddedPublicModels(index catalogIndex) []publicModel {
	models := make([]publicModel, 0, len(index.publicOrder))
	for _, id := range index.publicOrder {
		models = append(models, index.public[id])
	}
	return models
}

// catalogInfoForMode reports the read-only catalog status for the current mode.
// CLI mode always publishes the reviewed embedded snapshot; provider mode
// reports the live snapshot or the stale last-good fallback. The count is the
// effective published model count (whitelist and manual models applied), not a
// fixed snapshot size.
func (m *Manager) catalogInfoForMode(cfg Config) catalogInfo {
	if cfg.Mode == ModeCLI {
		return catalogInfo{
			Count:     len(m.publishedModels(cfg)),
			Source:    "embedded",
			FetchedAt: catalogdata.SnapshotDate,
			Stale:     false,
		}
	}
	_, info := m.catalogSnapshot(cfg)
	info.Count = len(m.publishedModels(cfg))
	return info
}

// refreshCatalog performs at most one anonymous GET /provider/v1/models per
// refreshInterval and atomically replaces the live snapshot on success. A failed
// refresh keeps the previous last-good snapshot and flags it stale; it is never
// cleared. A refresh that spans an authority or config-generation change is
// discarded so it cannot bind a snapshot to the wrong authority.
func (m *Manager) refreshCatalog(run chan struct{}, callbackID string) {
	cfg := m.config()
	base := cfg.BaseURL
	generation := m.configGeneration()

	m.catalog.mu.Lock()
	if m.catalog.refreshing {
		m.catalog.mu.Unlock()
		return
	}
	now := time.Now()
	if !m.catalog.lastAttempt.IsZero() && now.Sub(m.catalog.lastAttempt) < refreshInterval {
		m.catalog.mu.Unlock()
		return
	}
	m.catalog.refreshing = true
	m.catalog.lastAttempt = now
	m.catalog.mu.Unlock()

	models, errFetch := m.fetchPublicCatalog(run, callbackID, base)
	// Read the fresh config identity before taking catalog.mu so catalog.mu is
	// never held while acquiring m.mu (consistent lock order, no deadlock).
	freshGeneration := m.configGeneration()
	freshBase := m.config().BaseURL

	m.catalog.mu.Lock()
	defer m.catalog.mu.Unlock()
	m.catalog.refreshing = false
	// Discard a refresh whose authority or config generation changed mid-flight.
	if freshGeneration != generation || freshBase != base {
		return
	}
	if errFetch != nil || models == nil {
		m.catalog.lastFailed = true
		return
	}
	m.catalog.live = models
	m.catalog.liveAt = time.Now()
	m.catalog.liveBase = base
	m.catalog.liveGen = generation
	m.catalog.lastFailed = false
}

// fetchPublicCatalog reads the anonymous public catalog through the host under a
// bounded deadline. It is unauthenticated by design: it must never send a
// selected credential, and it always targets the supplied authority so a probe
// cannot follow a config change.
func (m *Manager) fetchPublicCatalog(run chan struct{}, callbackID, base string) ([]publicModel, error) {
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", PluginID+"/"+Version)
	resp, errDo := m.doHTTPDeadline(run, callbackID, http.MethodGet, joinURL(base, endpointProviderModels), headers, nil, nil, catalogHTTPDeadline)
	if errDo != nil {
		return nil, errDo
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstreamError(resp.StatusCode)
	}
	return decodePublicCatalog(resp.Body)
}

// catalogInvalid reports an unusable upstream catalog without echoing any of it.
func catalogInvalid() error {
	return &ProviderError{
		Code:       "invalid_upstream",
		Message:    "upstream model catalog is invalid",
		HTTPStatus: http.StatusBadGateway,
	}
}

// decodePublicCatalog validates one public catalog response. Native order and
// facts are preserved, the public prefix is applied exactly once by modelInfo,
// and duplicate ids are collapsed. A malformed response rejects the whole
// catalog so partial or invented data is never published. The entry count, id
// length and context length are all bounded so a hostile catalog cannot expand
// the published set or smuggle an invalid id/negative window.
func decodePublicCatalog(body []byte) ([]publicModel, error) {
	var decoded publicCatalog
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Data == nil {
		return nil, catalogInvalid()
	}
	if len(*decoded.Data) > maxCatalogEntries {
		return nil, catalogInvalid()
	}
	models := make([]publicModel, 0, len(*decoded.Data))
	seen := make(map[string]struct{}, len(*decoded.Data))
	for _, item := range *decoded.Data {
		native := nativeModelID(item.ID)
		if native == "" || len(native) > maxNativeModelIDLen || strings.IndexFunc(native, invalidNativeRune) >= 0 {
			return nil, catalogInvalid()
		}
		if item.ContextLength < 0 {
			return nil, catalogInvalid()
		}
		if _, ok := seen[native]; ok {
			continue
		}
		seen[native] = struct{}{}
		models = append(models, item)
	}
	return models, nil
}

// chatCapable reports whether a public entry declares at least one chat-capable
// endpoint. Decision/system models without a chat endpoint are never published.
func chatCapable(model publicModel) bool {
	for _, endpoint := range model.SupportedEndpoints {
		if _, ok := endpointFormat(endpoint); ok {
			return true
		}
	}
	return false
}

// publishedModels builds the effective, deterministic model catalog for the
// current mode. CLI mode publishes the reviewed CLI ∩ public intersection;
// provider mode publishes the current public snapshot (live or last-good).
// The configured whitelist narrows discovered models; manual models are then
// merged and may override discovered metadata.
func (m *Manager) publishedModels(cfg Config) []pluginapi.ModelInfo {
	index := loadEmbeddedIndex()
	whitelist := whitelistSet(cfg.Models)

	facts := make([]modelFacts, 0, len(index.publicOrder))
	if cfg.Mode == ModeProvider {
		models, _ := m.catalogSnapshot(cfg)
		for _, model := range models {
			if !chatCapable(model) {
				continue
			}
			native := nativeModelID(model.ID)
			cli, haveCLI := index.cli[native]
			var entry modelFacts
			if haveCLI {
				entry = factsFromCLI(cli)
			} else {
				entry = modelFacts{native: native}
			}
			entry = entry.mergePublic(model, haveCLI)
			if !allowed(whitelist, entry.native) {
				continue
			}
			facts = append(facts, entry)
		}
	} else {
		for _, native := range index.cliOrder {
			model, ok := index.public[native]
			if !ok {
				continue
			}
			// A reviewed CLI model whose public entry exposes no chat endpoint
			// (a system/decision endpoint) is neither published nor routable.
			if !chatCapable(model) {
				continue
			}
			if !allowed(whitelist, native) {
				continue
			}
			entry := factsFromCLI(index.cli[native]).mergePublic(model, true)
			facts = append(facts, entry)
		}
	}

	byNative := make(map[string]int, len(facts))
	for i, entry := range facts {
		byNative[entry.native] = i
	}
	for _, manual := range cfg.ManualModels {
		entry := modelFacts{native: manual.ID}
		if i, ok := byNative[manual.ID]; ok {
			entry = facts[i]
		} else {
			if cli, ok := index.cli[manual.ID]; ok {
				entry = factsFromCLI(cli)
			}
			if model, ok := index.public[manual.ID]; ok && chatCapable(model) {
				entry = entry.mergePublic(model, entry.name != "")
			}
		}
		entry = entry.applyManual(manual)
		if !manualEligible(cfg.Mode, manual, entry) {
			if i, ok := byNative[manual.ID]; ok {
				facts[i].native = ""
			}
			continue
		}
		if i, ok := byNative[manual.ID]; ok {
			facts[i] = entry
			continue
		}
		facts = append(facts, entry)
		byNative[manual.ID] = len(facts) - 1
	}

	models := make([]pluginapi.ModelInfo, 0, len(facts))
	for _, entry := range facts {
		if entry.native == "" {
			continue
		}
		models = append(models, entry.modelInfo())
	}
	return models
}

// manualEligible applies the same auth-mode gate to publication and execution.
// Provider models need a proven endpoint from the manual protocol or the
// current catalog. CLI models cannot declare a non text/image input.
func manualEligible(mode Mode, manual ManualModel, facts modelFacts) bool {
	if mode == ModeProvider {
		return len(facts.endpoints) > 0
	}
	for _, modality := range manual.InputModalities {
		if !cliModality(modality) {
			return false
		}
	}
	return true
}

func whitelistSet(models []string) map[string]bool {
	if len(models) == 0 {
		return nil
	}
	set := make(map[string]bool, len(models))
	for _, model := range models {
		set[model] = true
	}
	return set
}

func allowed(whitelist map[string]bool, native string) bool {
	if whitelist == nil {
		return true
	}
	return whitelist[native]
}

// lookupFacts resolves one native model for execution, applying manual overrides
// and returning false when the model is not part of the published catalog.
func (m *Manager) lookupFacts(cfg Config, native string) (modelFacts, bool) {
	index := loadEmbeddedIndex()
	var entry modelFacts
	found := false
	cli, haveCLI := index.cli[native]
	if haveCLI {
		entry = factsFromCLI(cli)
	}
	if cfg.Mode == ModeProvider {
		models, _ := m.catalogSnapshot(cfg)
		for _, model := range models {
			if nativeModelID(model.ID) != native {
				continue
			}
			// A system/decision entry with no chat endpoint is not routable.
			if !chatCapable(model) {
				break
			}
			entry = entry.mergePublic(model, haveCLI)
			found = true
			break
		}
		if !found {
			if model, ok := index.public[native]; ok && chatCapable(model) {
				entry = entry.mergePublic(model, haveCLI)
			}
		}
	} else if haveCLI {
		if model, ok := index.public[native]; ok {
			entry = entry.mergePublic(model, true)
		}
		found = true
	}
	var manual ManualModel
	manualFound := false
	for _, candidate := range cfg.ManualModels {
		if candidate.ID == native {
			manual = candidate
			entry = entry.applyManual(candidate)
			found = true
			manualFound = true
			break
		}
	}
	if manualFound && !manualEligible(cfg.Mode, manual, entry) {
		return modelFacts{}, false
	}
	if !found {
		return modelFacts{}, false
	}
	if !allowed(whitelistSet(cfg.Models), native) && !isManual(cfg.ManualModels, native) {
		return modelFacts{}, false
	}
	return entry, true
}

func isManual(models []ManualModel, native string) bool {
	for _, model := range models {
		if model.ID == native {
			return true
		}
	}
	return false
}

// endpointForFormat returns the public supported_endpoints value for a format.
func endpointForFormat(format translator.Format) string {
	switch format {
	case translator.FormatOpenAI:
		return "/chat/completions"
	case translator.FormatClaude:
		return "/messages"
	case translator.FormatOpenAIResponse:
		return "/responses"
	default:
		return ""
	}
}

// endpointDeclared reports whether a target format is declared by a model.
func endpointDeclared(endpoints []string, target translator.Format) bool {
	want := endpointForFormat(target)
	if want == "" {
		return false
	}
	for _, endpoint := range endpoints {
		if strings.TrimSpace(endpoint) == want {
			return true
		}
	}
	return false
}

// sdkFormatFor maps a wire protocol onto the SDK conversion format. The OpenAI
// Responses wire protocol is converted through the SDK "codex" format whose
// transformers are registered for the ordinary Responses API on both sides.
func sdkFormatFor(wire translator.Format) translator.Format {
	if wire == translator.FormatOpenAIResponse {
		return translator.FormatCodex
	}
	return wire
}

// translationSupported reports whether the SDK can bridge client<->upstream in
// both directions. The request direction is checked against the client request
// format and the response direction against the client response format, so a
// mismatch between them cannot authorize a conversion with no translator.
func translationSupported(clientReq, clientResp, target translator.Format, stream bool) bool {
	sdk := sdkFormatFor(target)
	if clientReq != target && !translator.HasRequestTransformer(clientReq, sdk) {
		return false
	}
	if clientResp == target {
		return true
	}
	if stream {
		return translator.HasStreamResponseTransformer(clientResp, sdk)
	}
	return translator.HasNonStreamResponseTransformer(clientResp, sdk)
}

// resolveTargetFormat selects a deterministic upstream endpoint format:
// the client's native protocol when the model declares it, otherwise the first
// declared endpoint (in fixed protocol order) that the SDK can bridge in both
// directions. A model that supports none fails closed.
func resolveTargetFormat(clientReq, clientResp translator.Format, endpoints []string, stream bool) (translator.Format, bool) {
	if clientReq == clientResp && endpointDeclared(endpoints, clientReq) && translationSupported(clientReq, clientResp, clientReq, stream) {
		return clientReq, true
	}
	for _, candidate := range []translator.Format{translator.FormatOpenAI, translator.FormatClaude, translator.FormatOpenAIResponse} {
		if !endpointDeclared(endpoints, candidate) {
			continue
		}
		if translationSupported(clientReq, clientResp, candidate, stream) {
			return candidate, true
		}
	}
	return "", false
}
