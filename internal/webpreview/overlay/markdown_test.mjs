import { test } from 'node:test';
import assert from 'node:assert/strict';
import { markdownBlocks, inlineMarkdown } from './markdown.js';

test('markdownBlocks projects headings, lists, quotes, paragraphs, and fenced code', () => {
  assert.deepEqual(markdownBlocks(`# Result

Done with **care**.

- first
- second

> useful note

\`\`\`js
const ok = true;
\`\`\``), [
    { type: 'heading', level: 1, text: 'Result' },
    { type: 'paragraph', text: 'Done with **care**.' },
    { type: 'list', ordered: false, items: ['first', 'second'] },
    { type: 'quote', text: 'useful note' },
    { type: 'code', language: 'js', text: 'const ok = true;' },
  ]);
});

test('inlineMarkdown recognizes emphasis, code, and safe links', () => {
  assert.deepEqual(inlineMarkdown('Use **bold**, *care*, `code`, and [docs](https://example.com).'), [
    { type: 'text', text: 'Use ' },
    { type: 'strong', text: 'bold' },
    { type: 'text', text: ', ' },
    { type: 'em', text: 'care' },
    { type: 'text', text: ', ' },
    { type: 'code', text: 'code' },
    { type: 'text', text: ', and ' },
    { type: 'link', text: 'docs', href: 'https://example.com' },
    { type: 'text', text: '.' },
  ]);
});

test('inlineMarkdown leaves unsafe links as plain text', () => {
  assert.deepEqual(inlineMarkdown('[nope](javascript:alert(1))'), [
    { type: 'text', text: '[nope](javascript:alert(1))' },
  ]);
});

test('markdownBlocks projects ordered lists', () => {
  assert.deepEqual(markdownBlocks('1. first\n2. second'), [
    { type: 'list', ordered: true, items: ['first', 'second'] },
  ]);
});

test('markdownBlocks does not hang on a malformed fence opener', { timeout: 2000 }, () => {
  assert.deepEqual(markdownBlocks('```js extra\nnext line'), [
    { type: 'paragraph', text: '```js extra' },
    { type: 'paragraph', text: 'next line' },
  ]);
});
