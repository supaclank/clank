import { toolSummary } from './chat.js';
import { markdownBlocks, inlineMarkdown } from './markdown.js';

const appendInlineMarkdown = (parent, text) => {
  for (const token of inlineMarkdown(text)) {
    if (token.type === 'text') { parent.append(document.createTextNode(token.text)); continue; }
    const tag = token.type === 'strong' ? 'strong' : token.type === 'em' ? 'em' : token.type === 'code' ? 'code' : 'a';
    const el = document.createElement(tag);
    el.textContent = token.text;
    if (token.type === 'link') {
      el.href = token.href;
      el.target = '_blank';
      el.rel = 'noopener noreferrer';
    }
    parent.append(el);
  }
};

export const renderMarkdown = (text) => {
  const container = document.createElement('div');
  container.className = 'md';
  for (const block of markdownBlocks(text)) {
    let el;
    if (block.type === 'heading') el = document.createElement(`h${block.level}`);
    else if (block.type === 'quote') el = document.createElement('blockquote');
    else if (block.type === 'code') {
      el = document.createElement('pre');
      const code = document.createElement('code');
      code.textContent = block.text;
      if (block.language) code.dataset.language = block.language;
      el.append(code);
      container.append(el);
      continue;
    } else if (block.type === 'list') {
      el = document.createElement(block.ordered ? 'ol' : 'ul');
      for (const item of block.items) {
        const li = document.createElement('li');
        appendInlineMarkdown(li, item);
        el.append(li);
      }
      container.append(el);
      continue;
    } else el = document.createElement('p');
    appendInlineMarkdown(el, block.text);
    container.append(el);
  }
  return container;
};

const cardChevron = (icons) => {
  const el = document.createElement('span');
  el.className = 'card-chevron';
  el.innerHTML = icons.chevron;
  return el;
};

const renderThinking = (row, open, icons, onToggle) => {
  const card = document.createElement('div');
  card.className = 'transcript-card thinking-card' + (open ? ' open' : '');
  const button = document.createElement('button');
  button.type = 'button';
  button.setAttribute('aria-expanded', String(open));
  button.setAttribute('aria-label', `${open ? 'Hide' : 'Show'} agent thinking`);
  const icon = document.createElement('span'); icon.className = 'card-icon'; icon.textContent = '◌';
  const name = document.createElement('span'); name.className = 'card-name'; name.textContent = 'Thinking';
  const summary = document.createElement('span'); summary.className = 'card-summary'; summary.textContent = row.text.replace(/\s+/g, ' ').trim();
  button.append(icon, name, summary, cardChevron(icons));
  button.onclick = () => onToggle(row.id);
  card.append(button);
  if (open) {
    const details = document.createElement('div');
    details.className = 'card-details';
    details.append(renderMarkdown(row.text));
    card.append(details);
  }
  return card;
};

const toolValueText = (value) => {
  if (typeof value === 'string') return value;
  try { return JSON.stringify(value, null, 2); } catch { return String(value); }
};

const renderToolCall = (row, open, icons, onToggle) => {
  const card = document.createElement('div');
  card.className = 'transcript-card tool-card' + (open ? ' open' : '');
  const button = document.createElement('button');
  button.type = 'button';
  button.setAttribute('aria-expanded', String(open));
  button.setAttribute('aria-label', `${open ? 'Hide' : 'Show'} ${row.tool} tool details`);
  const state = document.createElement('span'); state.className = `tool-state ${row.status}`;
  const name = document.createElement('span'); name.className = 'card-name'; name.textContent = row.tool;
  const summary = document.createElement('span'); summary.className = 'card-summary'; summary.textContent = toolSummary(row);
  button.append(state, name, summary, cardChevron(icons));
  button.onclick = () => onToggle(row.id);
  card.append(button);
  if (open) {
    const details = document.createElement('div');
    details.className = 'card-details';
    for (const [label, value] of [['Input', row.input], ['Output', row.output]]) {
      if (value === undefined) continue;
      const section = document.createElement('div'); section.className = 'tool-section';
      const title = document.createElement('b'); title.textContent = label;
      const pre = document.createElement('pre'); pre.textContent = toolValueText(value);
      section.append(title, pre); details.append(section);
    }
    card.append(details);
  }
  return card;
};

export const createTranscriptRenderer = ({ icons, isExpanded, onToggle }) => (row) => {
  if (row.kind === 'thinking') return renderThinking(row, isExpanded(row.id), icons, onToggle);
  if (row.kind === 'tool') return renderToolCall(row, isExpanded(row.id), icons, onToggle);
  const el = document.createElement('div');
  el.className = 'm ' + row.role;
  const who = document.createElement('span');
  who.className = 'who';
  who.textContent = row.role === 'user' ? 'you' : 'clank';
  const body = document.createElement('div');
  body.className = 'body';
  if (row.role === 'assistant') body.append(renderMarkdown(row.text));
  else body.textContent = row.text;
  el.append(who, body);
  return el;
};
