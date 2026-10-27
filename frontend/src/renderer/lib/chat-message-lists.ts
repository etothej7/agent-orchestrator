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
 * - A numbered line only STARTS a list when its number is 1, or it follows a list
 *   line, or the next line is numbered too. "2024. was a year" stays a sentence.
 * - A thematic break (`* * *`, `- - -`, `***`) is never a list item.
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
/** A horizontal rule: three or more of one of `-`, `*`, `_`, spaces allowed between them. */
const THEMATIC_BREAK = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/;

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
	// The last non-blank line was part of a list (an item or its continuation), even if a blank line has
	// since ended that list: a numbered line after it continues the document's list rather than opening one.
	let afterListLine = false;

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

	const lines = text.split("\n");
	for (const [index, line] of lines.entries()) {
		const fenceMatch = FENCE.exec(line);
		if (fence !== null) {
			pending.push(line);
			if (fenceMatch && fenceMatch[1] === fence) fence = null;
			continue;
		}
		if (fenceMatch) {
			flushList();
			afterListLine = false;
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
		const thematic = THEMATIC_BREAK.test(line);
		const bullet = thematic ? null : BULLET_ITEM.exec(line);
		let ordered = bullet || thematic ? null : ORDERED_ITEM.exec(line);
		if (ordered && !(list?.ordered) && !startsOrderedList(ordered, afterListLine, lines, index)) ordered = null;
		if (bullet || ordered) {
			afterListLine = true;
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
		if (line.trim() !== "") afterListLine = false;
	}
	flushList();
	flushText();
	return blocks;
}

/**
 * Whether a numbered line that is not continuing an ordered list opens one: its number is 1, it follows a
 * list line, or the next line is a numbered line too (two in a row are a list, whatever their numbers). Lines
 * indented under the item belong to it, so the "next line" is the first one after them.
 */
function startsOrderedList(match: RegExpExecArray, afterListLine: boolean, lines: string[], index: number): boolean {
	if (Number(match[1]) === 1 || afterListLine) return true;
	let next = index + 1;
	while (next < lines.length && INDENTED.test(lines[next] ?? "")) next += 1;
	return ORDERED_ITEM.test(lines[next] ?? "");
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
