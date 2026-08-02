import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  resolvePreset, applyPresetOverrides, configRows, setConfigOverride,
  diffConfigAgainstOptions, effectiveSessionConfig, profileLabel,
  profileSavePayload,
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
