// settings.js — pure agent-profile logic for the preview overlay.
//
// The DOM and network stay in overlay.js. Keeping selection, override
// normalization, and live-session diffs here makes the mobile-parity rules
// executable under node --test without a browser or mocked dependencies.

// Mirrors internal/agent/presets BuiltinDefaultPrefix.
export const BUILTIN_DEFAULT_PREFIX = 'builtin-default-';

// resolvePreset mirrors clank-mobile's create editor precedence: the exact
// saved pick, then the backend's built-in Build profile, then the first
// profile for that backend. It never crosses backend boundaries.
export const resolvePreset = (presetList, backend, presetID) => {
  if (!Array.isArray(presetList) || !backend) return null;
  const profiles = presetList.filter((p) => p && p.backend === backend && p.config);
  return profiles.find((p) => p.id === presetID) ||
    profiles.find((p) => p.id === BUILTIN_DEFAULT_PREFIX + backend) ||
    profiles[0] || null;
};

// applyPresetOverrides assembles the create-time config. The selected
// profile remains the complete baseline; overrides only carry divergence.
export const applyPresetOverrides = (preset, overrides) =>
  preset ? { ...preset.config, ...(overrides || {}) } : null;

// setConfigOverride keeps a create draft normalized: choosing the profile's
// own value removes the override instead of storing a redundant value.
export const setConfigOverride = (preset, overrides, id, value) => {
  const next = { ...(overrides || {}) };
  if (preset && preset.config && preset.config[id] === value) delete next[id];
  else next[id] = value;
  return next;
};

// profileLabel is the compact footer-chip label used by the mobile editor.
export const profileLabel = (preset, overrides) => {
  if (!preset) return '';
  return Object.keys(overrides || {}).some((id) => overrides[id] !== preset.config[id])
    ? 'Custom'
    : preset.name;
};

// configRows merges live agent options with the selected profile. Agent
// order wins; profile-only keys remain visible so nothing being sent is
// hidden just because an adapter stopped advertising it.
export const configRows = (preset, overrides, options) => {
  const rows = [];
  const seen = new Set();
  for (const option of options || []) {
    seen.add(option.id);
    let value;
    let source;
    if (Object.hasOwn(overrides || {}, option.id)) {
      value = overrides[option.id];
      source = 'override';
    } else if (preset && Object.hasOwn(preset.config || {}, option.id)) {
      value = preset.config[option.id];
      source = 'preset';
    } else {
      value = option.current_value || '';
      source = 'agent';
    }
    const values = option.values || [];
    rows.push({
      id: option.id,
      name: option.name || option.id,
      value,
      valueName: (values.find((v) => v.value === value) || {}).name || value,
      source,
      values,
    });
  }
  for (const id of Object.keys((preset && preset.config) || {}).filter((id) => !seen.has(id)).sort()) {
    const overridden = Object.hasOwn(overrides || {}, id);
    const value = overridden ? overrides[id] : preset.config[id];
    rows.push({
      id,
      name: id,
      value,
      valueName: value,
      source: overridden ? 'override' : 'preset',
      values: [],
    });
  }
  return rows;
};

// The well-known option id every serving agent uses for its session mode.
export const CONFIG_OPTION_MODE = 'mode';

// profileMatchingConfig mirrors clank-mobile's presetMatchingConfig: the
// profile the session's EFFECTIVE config verifiably embodies, or null.
// Honesty rules: every key the profile promises must be verifiable and
// equal; empty-config (instructions-only) profiles never match; first
// match wins. Per-key precedence: staged override (tap feedback) →
// advertised current value (live agent truth) → persisted (clank-host's
// last-applied config, the only source an undecorated session has).
// Advertised beats persisted by design: a value the agent refused must
// not fake a match.
export const profileMatchingConfig = (profiles, options, overrides, persisted) => {
  const list = (Array.isArray(profiles) ? profiles : []).filter((p) => p && p.config);
  return list.find((p) => {
    const keys = Object.keys(p.config);
    if (!keys.length) return false;
    return keys.every((id) => {
      let effective;
      if (Object.hasOwn(overrides || {}, id)) {
        effective = overrides[id];
      } else {
        const option = (options || []).find((o) => o.id === id);
        effective = option ? option.current_value : undefined;
        if (effective === undefined || effective === null) effective = (persisted || {})[id];
      }
      return effective !== undefined && effective === p.config[id];
    });
  }) || null;
};

// liveChipLabel is the in-session footer-chip label: the matched
// profile's name — same vocabulary as the create chip — falling back to
// the agent's mode value name, then to the persisted raw mode id
// (undecorated session), then ''. The caller supplies the generic
// fallback text and the staged-change dot.
export const liveChipLabel = (profiles, options, pending, persisted) => {
  const matched = profileMatchingConfig(profiles, options, pending, persisted);
  if (matched) return matched.name;
  const mode = (options || []).find((o) =>
    o.category === CONFIG_OPTION_MODE || o.id === CONFIG_OPTION_MODE);
  if (!mode) return (persisted || {})[CONFIG_OPTION_MODE] || '';
  const value = Object.hasOwn(pending || {}, mode.id) ? pending[mode.id] : mode.current_value;
  return ((mode.values || []).find((v) => v.value === value) || {}).name || value || '';
};

// diffConfigAgainstOptions enforces DATA-040 for a live session: omitted
// means unchanged, so values already active on the agent must not be sent.
export const diffConfigAgainstOptions = (config, options) => {
  const out = {};
  for (const [id, value] of Object.entries(config || {})) {
    const option = (options || []).find((o) => o.id === id);
    if (!option || option.current_value !== value) out[id] = value;
  }
  return out;
};

// Materialize the live agent state from current values plus staged changes.
// Unadvertised staged keys survive.
export const effectiveSessionConfig = (options, pending) => {
  const current = {};
  for (const option of options || []) current[option.id] = option.current_value;
  return { ...current, ...(pending || {}) };
};

const BUILTIN_ID_PREFIX = 'builtin-';

const slugifyProfileID = (name) => name
  .trim()
  .toLowerCase()
  .replace(/[^a-z0-9]+/g, '-')
  .replace(/^-+|-+$/g, '');

// profileSavePayload mirrors mobile savePayload: a stable URL-safe id from
// the display name, with built-in ids reserved for host-shipped profiles.
export const profileSavePayload = (name, backend, config) => {
  const trimmed = name.trim();
  const id = slugifyProfileID(trimmed);
  if (!id) throw new Error('Give the profile a name first');
  if (id.startsWith(BUILTIN_ID_PREFIX)) {
    throw new Error('Profile names starting with "builtin-" are reserved');
  }
  if (!backend) throw new Error('Cannot save a profile without a backend');
  if (!config || !Object.keys(config).length) throw new Error('Cannot save an empty profile');
  return { id, name: trimmed, backend, config: { ...config } };
};
