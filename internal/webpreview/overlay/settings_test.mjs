import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  resolvePreset, applyPresetOverrides, configRows, setConfigOverride,
  diffConfigAgainstOptions, effectiveSessionConfig, mergeSessionConfig, profileLabel,
  profileMatchingConfig, liveChipLabel, liveSettingsBadge, profileSavePayload,
  canSetDefaultProfile,
} from './settings.js';

test('resolvePreset: exact pick, then builtin Build, without accepting another backend', () => {
  const presets = [
    { id: 'builtin-default-claude-code', name: 'Build', backend: 'claude-code', config: { mode: 'auto' } },
    { id: 'mine', name: 'Careful', backend: 'claude-code', config: { mode: 'plan' } },
    { id: 'mine-opencode', name: 'Other', backend: 'opencode', config: { mode: 'build' } },
  ];
  assert.equal(resolvePreset(presets, 'claude-code', 'mine').id, 'mine');
  assert.equal(resolvePreset(presets, 'claude-code', 'stale').id, 'builtin-default-claude-code');
  assert.equal(resolvePreset(presets, 'codex', 'mine-opencode'), null);
});

test('profile config: overrides stay normalized and label becomes Custom', () => {
  const preset = { name: 'Build', config: { mode: 'auto', effort: 'default' } };
  let overrides = setConfigOverride(preset, {}, 'effort', 'high');
  assert.deepEqual(overrides, { effort: 'high' });
  assert.equal(profileLabel(preset, overrides), 'Custom');
  const config = applyPresetOverrides(preset, overrides);
  assert.deepEqual(config, { mode: 'auto', effort: 'high' });
  config.mode = 'mutated';
  assert.equal(preset.config.mode, 'auto');
  overrides = setConfigOverride(preset, overrides, 'effort', 'default');
  assert.deepEqual(overrides, {});
  assert.equal(profileLabel(preset, overrides), 'Build');
});

test('canSetDefaultProfile: false for live sessions, custom overrides, the current default, and a "+ New" draft', () => {
  const preset = { id: 'mine', name: 'Careful', config: { mode: 'plan' } };
  const otherDefault = { id: 'builtin-default-claude-code' };
  assert.equal(canSetDefaultProfile(preset, otherDefault, { live: false, custom: false, draft: false }), true);
  assert.equal(canSetDefaultProfile(preset, otherDefault, { live: true, custom: false, draft: false }), false);
  assert.equal(canSetDefaultProfile(preset, otherDefault, { live: false, custom: true, draft: false }), false);
  assert.equal(canSetDefaultProfile(preset, preset, { live: false, custom: false, draft: false }), false);
  // "+ New" leaves `preset` pointing at the previously selected profile —
  // without the draft check this would offer to save it as default.
  assert.equal(canSetDefaultProfile(preset, otherDefault, { live: false, custom: false, draft: true }), false);
});

test('configRows: agent order, effective provenance, and preset-only keys', () => {
  const preset = { config: { mode: 'auto', instructions: 'strict' } };
  const options = [{
    id: 'mode', name: 'Mode', current_value: 'default',
    values: [{ value: 'default', name: 'Manual' }, { value: 'auto', name: 'Auto' }],
  }, {
    id: 'effort', name: 'Effort', current_value: 'medium',
    values: [{ value: 'medium', name: 'Medium' }, { value: 'high', name: 'High' }],
  }];
  assert.deepEqual(configRows(preset, { effort: 'high' }, options), [
    { id: 'mode', name: 'Mode', value: 'auto', valueName: 'Auto', source: 'preset', values: options[0].values },
    { id: 'effort', name: 'Effort', value: 'high', valueName: 'High', source: 'override', values: options[1].values },
    { id: 'instructions', name: 'instructions', value: 'strict', valueName: 'strict', source: 'preset', values: [] },
  ]);
});

test('diffConfigAgainstOptions: follow-up sends changes only', () => {
  const options = [
    { id: 'mode', current_value: 'auto' },
    { id: 'effort', current_value: 'medium' },
  ];
  assert.deepEqual(diffConfigAgainstOptions(
    { mode: 'auto', effort: 'high', unadvertised: 'kept' },
    options,
  ), { effort: 'high', unadvertised: 'kept' });
});

test('effectiveSessionConfig: current agent values plus staged and unadvertised changes', () => {
  const options = [
    { id: 'mode', current_value: 'auto' },
    { id: 'effort', current_value: 'medium' },
  ];
  assert.deepEqual(effectiveSessionConfig(options, {
    effort: 'high',
    custom_key: 'kept',
  }), {
    mode: 'auto',
    effort: 'high',
    custom_key: 'kept',
  });
});

test('mergeSessionConfig: empty-string changes are skipped, like the host', () => {
  // The host's recordSessionConfig drops empty keys/values rather than
  // persisting "" as a real value; the overlay must match or a cleared
  // knob would read back as an empty-string current value instead of gone.
  assert.deepEqual(
    mergeSessionConfig({ mode: 'auto' }, { mode: 'plan', effort: '' }),
    { mode: 'plan' },
  );
  assert.deepEqual(mergeSessionConfig({ mode: 'auto' }, { '': 'x' }), { mode: 'auto' });
  assert.deepEqual(mergeSessionConfig(null, { mode: 'plan' }), { mode: 'plan' });
});

test('profileSavePayload: slugifies the name and copies the effective config', () => {
  const config = { mode: 'auto', effort: 'high' };
  const payload = profileSavePayload('  Careful Review  ', 'claude-code', config);
  assert.deepEqual(payload, {
    id: 'careful-review',
    name: 'Careful Review',
    backend: 'claude-code',
    config,
  });
  payload.config.mode = 'mutated';
  assert.equal(config.mode, 'auto');
  assert.throws(() => profileSavePayload('  ', 'claude-code', config), /name/);
  assert.throws(() => profileSavePayload('builtin-secret', 'claude-code', config), /reserved/);
});

test('profileMatchingConfig: staged and advertised sources, honesty rules', () => {
  const build = { id: 'b', name: 'Build', backend: 'claude-code', config: { mode: 'auto', model: 'default', effort: 'default' } };
  const opusHigh = { id: 'oh', name: 'Opus High', backend: 'claude-code', config: { mode: 'bypassPermissions', model: 'opus', effort: 'high' } };
  const promptOnly = { id: 'p', name: 'Prompt only', backend: 'claude-code', config: {}, instructions: 'be terse' };
  const options = [
    { id: 'mode', category: 'mode', current_value: 'auto', values: [] },
    { id: 'model', category: 'model', current_value: 'default', values: [] },
    { id: 'effort', current_value: 'default', values: [] },
  ];
  // Advertised current values verify Build.
  assert.equal(profileMatchingConfig([opusHigh, build], options, {}, {}).id, 'b');
  // Staged overrides count — tapping Opus High relabels before the send.
  assert.equal(
    profileMatchingConfig([build, opusHigh], options,
      { mode: 'bypassPermissions', model: 'opus', effort: 'high' }, {}).id,
    'oh',
  );
  // An unverifiable key disqualifies; instructions-only never matches.
  assert.equal(profileMatchingConfig([opusHigh], options, {}, {}), null);
  assert.equal(profileMatchingConfig([promptOnly], options, {}, {}), null);
});

test('profileMatchingConfig: persisted is last resort, advertised beats it', () => {
  const opusHigh = { id: 'oh', name: 'Opus High', backend: 'claude-code', config: { mode: 'bypassPermissions', model: 'opus', effort: 'high' } };
  // Undecorated session (no advertised options): persisted alone matches.
  assert.equal(profileMatchingConfig([opusHigh], null, {}, opusHigh.config).id, 'oh');
  // A live agent that says sonnet must not be faked over by a persisted
  // opus (e.g. a value the agent refused).
  const live = [
    { id: 'mode', category: 'mode', current_value: 'bypassPermissions', values: [] },
    { id: 'model', category: 'model', current_value: 'sonnet', values: [] },
    { id: 'effort', current_value: 'high', values: [] },
  ];
  assert.equal(profileMatchingConfig([opusHigh], live, {}, opusHigh.config), null);
  // Persisted fills keys the live set does not advertise.
  const partial = [{ id: 'model', category: 'model', current_value: 'opus', values: [] }];
  assert.equal(
    profileMatchingConfig([opusHigh], partial, {}, { mode: 'bypassPermissions', effort: 'high' }).id,
    'oh',
  );
});

test('liveChipLabel: profile name first, mode name fallback, persisted raw mode last', () => {
  const build = { id: 'b', name: 'Build', backend: 'claude-code', config: { mode: 'auto', model: 'default', effort: 'default' } };
  const options = [
    { id: 'mode', category: 'mode', current_value: 'auto',
      values: [{ value: 'auto', name: 'Auto' }, { value: 'plan', name: 'Plan Mode' }] },
    { id: 'model', category: 'model', current_value: 'default', values: [] },
    { id: 'effort', current_value: 'default', values: [] },
  ];
  // The regression: after sending with Build, the chip must keep saying
  // Build — not flip to the mode's display name ("Auto").
  assert.equal(liveChipLabel([build], options, {}, {}), 'Build');
  // No profile verifiably matches → the honest mode-name fallback.
  const drifted = options.map((o) => (o.id === 'effort' ? { ...o, current_value: 'high' } : o));
  assert.equal(liveChipLabel([build], drifted, {}, {}), 'Auto');
  // Staged mode wins the fallback value.
  assert.equal(liveChipLabel([build], drifted, { mode: 'plan' }, {}), 'Plan Mode');
  // Undecorated session: persisted names the profile; raw mode id when
  // nothing matches; '' when nothing is known at all.
  assert.equal(liveChipLabel([build], null, {}, build.config), 'Build');
  assert.equal(liveChipLabel([build], null, {}, { mode: 'plan' }), 'plan');
  assert.equal(liveChipLabel([build], null, {}, {}), '');
});

test('liveSettingsBadge: profile picks are not modifications', () => {
  const build = { id: 'b', name: 'Build', backend: 'claude-code', config: { mode: 'auto' } };
  // A staged profile pick matches → no badge (card + chip carry it).
  assert.equal(liveSettingsBadge({ mode: 'auto' }, build), '');
  // Manual divergence from every profile → Modified.
  assert.equal(liveSettingsBadge({ effort: 'max' }, null), 'Modified');
  // Nothing staged → no badge, matched or not.
  assert.equal(liveSettingsBadge({}, null), '');
  assert.equal(liveSettingsBadge({}, build), '');
});

test('liveSettingsBadge: Draft wins while the + New card is selected', () => {
  const build = { id: 'b', name: 'Build', backend: 'claude-code', config: { mode: 'auto' } };
  // Draft regardless of staged state or matches — the card is selected.
  assert.equal(liveSettingsBadge({}, null, true), 'Draft');
  assert.equal(liveSettingsBadge({ mode: 'auto' }, build, true), 'Draft');
  // Without the draft the existing rules hold.
  assert.equal(liveSettingsBadge({ effort: 'max' }, null, false), 'Modified');
  assert.equal(liveSettingsBadge({ mode: 'auto' }, build, false), '');
});
