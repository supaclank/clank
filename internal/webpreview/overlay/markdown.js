// A deliberately small Markdown projection for chat. It returns data rather
// than HTML so overlay.js can build DOM nodes without an injection surface.

const SAFE_LINK = /^(https?:|mailto:)/i;

export const inlineMarkdown = (source) => {
  const text = String(source || '');
  const tokens = [];
  const pushText = (value) => {
    if (!value) return;
    const prior = tokens[tokens.length - 1];
    if (prior && prior.type === 'text') prior.text += value;
    else tokens.push({ type: 'text', text: value });
  };
  const pattern = /(\[([^\]]+)\]\(([^)]+)\)|\*\*([^*]+)\*\*|`([^`]+)`|\*([^*]+)\*)/g;
  let cursor = 0;
  let match;
  while ((match = pattern.exec(text))) {
    if (match.index > cursor) pushText(text.slice(cursor, match.index));
    if (match[2] !== undefined && SAFE_LINK.test(match[3])) {
      tokens.push({ type: 'link', text: match[2], href: match[3] });
    } else if (match[4] !== undefined) {
      tokens.push({ type: 'strong', text: match[4] });
    } else if (match[5] !== undefined) {
      tokens.push({ type: 'code', text: match[5] });
    } else if (match[6] !== undefined) {
      tokens.push({ type: 'em', text: match[6] });
    } else {
      pushText(match[0]);
    }
    cursor = pattern.lastIndex;
  }
  if (cursor < text.length) pushText(text.slice(cursor));
  return tokens.length ? tokens : [{ type: 'text', text }];
};

export const markdownBlocks = (source) => {
  const lines = String(source || '').replace(/\r\n?/g, '\n').split('\n');
  const blocks = [];
  for (let i = 0; i < lines.length;) {
    const line = lines[i];
    if (!line.trim()) { i++; continue; }
    const fence = line.match(/^```\s*([\w+-]*)\s*$/);
    if (fence) {
      const code = [];
      i++;
      while (i < lines.length && !/^```\s*$/.test(lines[i])) code.push(lines[i++]);
      if (i < lines.length) i++;
      blocks.push({ type: 'code', language: fence[1], text: code.join('\n') });
      continue;
    }
    const heading = line.match(/^(#{1,6})\s+(.+)$/);
    if (heading) {
      blocks.push({ type: 'heading', level: heading[1].length, text: heading[2] });
      i++;
      continue;
    }
    const list = line.match(/^\s*(?:(\d+)\.|([-+*]))\s+(.+)$/);
    if (list) {
      const ordered = list[1] !== undefined;
      const items = [];
      while (i < lines.length) {
        const item = lines[i].match(/^\s*(?:(\d+)\.|([-+*]))\s+(.+)$/);
        if (!item || (item[1] !== undefined) !== ordered) break;
        items.push(item[3]);
        i++;
      }
      blocks.push({ type: 'list', ordered, items });
      continue;
    }
    if (/^>\s?/.test(line)) {
      const quote = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) quote.push(lines[i++].replace(/^>\s?/, ''));
      blocks.push({ type: 'quote', text: quote.join('\n') });
      continue;
    }
    const paragraph = [];
    while (i < lines.length && lines[i].trim() && !/^```/.test(lines[i]) &&
      !/^(#{1,6})\s+/.test(lines[i]) && !/^\s*(?:(\d+)\.|[-+*])\s+/.test(lines[i]) && !/^>\s?/.test(lines[i])) {
      paragraph.push(lines[i++]);
    }
    blocks.push({ type: 'paragraph', text: paragraph.join('\n') });
  }
  return blocks;
};
