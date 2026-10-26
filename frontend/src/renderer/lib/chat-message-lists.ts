/**
 * Lists in the messages a person sends.
 *
 * Ed writes "- a" and "1. b" lines in his chat messages to agents. The agent
 * receives exactly those characters; this module only decides how the sent text
 * is *drawn*. It recognises list items and nothing else, on purpose: running a
 * full markdown renderer over a person's own words would also swallow `**`, `_`
 * and `5 * 3`, which changes what he sees without changing what was sent.
 *
 * - Item markers are `-`, `*` or `+` (bullets) and `1.` / `1)` (numbered), at the
 *   start of a line or after up to three spaces.
 * - A line indented two spaces or more under an item belongs to it: more of its
 *   text, or a nested list (up to three levels deep).
 * - A blank line, or any other line, ends the list.
 * - Nothing inside a fenced code block is a list.
 * - Everything that is not a list item stays text, byte for byte.
 *
 * The result is plain data for React to render as text nodes, so there is no HTML
 * path to sanitize.
 */

/** One list item is its own blocks: its text first, then any nested list under it. */
export type MessageBlock =
	| { kind: "text"; text: string }
	| { kind: "list"; ordered: boolean; start: number; items: MessageBlock[][] };

const BULLET_ITEM = /^ {0,3}[-*+][ \t]+(\S.*)$/;
const ORDERED_ITEM = /^ {0,3}(\d{1,9})[.)][ \t]+(\S.*)$/;
const INDENTED = /^(?: {2,}|\t)\S/;
const FENCE = /^ {0,3}(```|~~~)/;

const MAX_DEPTH = 3;

/** `indent` is how far an item's own lines are indented under its marker. */
type RawList = { ordered: boolean; start: number; items: { indent: number; lines: string[] }[] };

/** Split sent text into text and list blocks. Text with no list returns one text block, unchanged. */
export function splitMessageLists(text: string, depth = 0): MessageBlock[] {
	if (depth >= MAX_DEPTH || !/(^|\n) {0,3}(?:[-*+]|\d{1,9}[.)])[ \t]+\S/.test(text)) return [{ kind: "text", text }];
	const blocks: MessageBlock[] = [];
	let pending: string[] = [];
	let list: RawList | null = null;
	let fence: string | null = null;

	const flushText = () => {
		if (pending.length === 0) return;
		// A blank line next to a list is spacing the list's own margin already provides.
		while (pending.length > 0 && pending[pending.length - 1] === "") pending.pop();
		if (blocks[blocks.length - 1]?.kind === "list") {
			while (pending.length > 0 && pending[0] === "") pending.shift();
		}
		if (pending.length > 0) blocks.push({ kind: "text", text: pending.join("\n") });
		pending = [];
	};
	const flushList = () => {
		if (list) {
			blocks.push({
				kind: "list",
				ordered: list.ordered,
				start: list.start,
				items: list.items.map((item) => splitMessageLists(item.lines.join("\n"), depth + 1)),
			});
		}
		list = null;
	};

	for (const line of text.split("\n")) {
		const fenceMatch = FENCE.exec(line);
		if (fence !== null) {
			pending.push(line);
			if (fenceMatch && fenceMatch[1] === fence) fence = null;
			continue;
		}
		if (fenceMatch) {
			flushList();
			pending.push(line);
			fence = fenceMatch[1] ?? null;
			continue;
		}
		if (list && INDENTED.test(line)) {
			// Part of the item above: more text, or a nested list once dedented.
			const item = list.items[list.items.length - 1];
			if (item) item.lines.push(line.replace(new RegExp(`^(?: {1,${item.indent}}|\\t)`), ""));
			continue;
		}
		const bullet = BULLET_ITEM.exec(line);
		const ordered = bullet ? null : ORDERED_ITEM.exec(line);
		if (bullet || ordered) {
			const wantsOrdered = ordered !== null;
			if (!list || list.ordered !== wantsOrdered) {
				flushList();
				flushText();
				list = { ordered: wantsOrdered, start: ordered ? Number(ordered[1]) : 1, items: [] };
			}
			const marker = (bullet ?? ordered)?.[0] ?? "";
			const content = (bullet ? bullet[1] : ordered?.[2]) ?? "";
			list.items.push({ indent: Math.max(marker.length - content.length, 2), lines: [content] });
			continue;
		}
		flushList();
		pending.push(line);
	}
	flushList();
	flushText();
	return blocks;
}

const COMPOSER_LIST_LINE = /^( {0,3})(?:([-*+])|(\d{1,9})([.)]))( +)(.*)$/;

export type ListContinuation =
	/** Start the next item with this text after a line break. */
	| { kind: "continue"; prefix: string }
	/** The item is empty: take the marker away and end the list. */
	| { kind: "end" };

/**
 * What Shift+Enter should do at the end of a composer line that is a list item.
 * Enter sends, so the list continues on Shift+Enter, which already makes a newline.
 * A caret in the middle of a line, or a line that is not a list item, keeps the
 * plain newline.
 */
export function listContinuation(lineBeforeCaret: string, lineAfterCaret: string): ListContinuation | null {
	if (lineAfterCaret.trim() !== "") return null;
	const match = COMPOSER_LIST_LINE.exec(lineBeforeCaret);
	if (!match) return null;
	const [, indent = "", bullet, number, delimiter = ".", gap = " ", content = ""] = match;
	if (content.trim() === "") return { kind: "end" };
	const marker = bullet ?? `${Number(number) + 1}${delimiter}`;
	return { kind: "continue", prefix: `${indent}${marker}${gap}` };
}
