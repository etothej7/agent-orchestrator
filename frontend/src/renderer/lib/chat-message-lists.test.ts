import { describe, expect, it } from "vitest";
import { listContinuation, splitMessageLists, type MessageBlock } from "./chat-message-lists";

const text = (value: string): MessageBlock => ({ kind: "text", text: value });
const list = (ordered: boolean, start: number, ...items: MessageBlock[][]): MessageBlock => ({ kind: "list", ordered, start, items });
const bullets = (...items: string[]): MessageBlock => list(false, 1, ...items.map((item) => [text(item)]));

describe("splitMessageLists", () => {
	it("leaves text without a list as one unchanged text block", () => {
		for (const sample of [
			"",
			"plain words",
			"line one\nline two\n\nline four",
			"a-b and 1.5 and - not at line start",
			"**bold** _em_ 5 * 3 <b>x</b>",
			"-nospace\n1.nospace",
			"- \n1. ",
		]) {
			expect(splitMessageLists(sample)).toEqual([text(sample)]);
		}
	});

	it("turns dash, star and plus lines into one bullet list", () => {
		expect(splitMessageLists("- a\n* b\n+ c")).toEqual([bullets("a", "b", "c")]);
	});

	it("turns numbered lines into an ordered list that keeps its first number", () => {
		expect(splitMessageLists("1. a\n2) b")).toEqual([list(true, 1, [text("a")], [text("b")])]);
		expect(splitMessageLists("3. c\n4. d")).toEqual([list(true, 3, [text("c")], [text("d")])]);
	});

	it("keeps prose before and after a list as text", () => {
		expect(splitMessageLists("Please do:\n- a\n- b\nThanks")).toEqual([text("Please do:"), bullets("a", "b"), text("Thanks")]);
	});

	it("starts a new list when the kind changes or a blank line intervenes", () => {
		expect(splitMessageLists("- a\n1. b")).toEqual([bullets("a"), list(true, 1, [text("b")])]);
		expect(splitMessageLists("1. a\n\n2. b")).toEqual([list(true, 1, [text("a")]), list(true, 2, [text("b")])]);
	});

	it("attaches indented lines to the item above and nests indented markers", () => {
		expect(splitMessageLists("- a\n  more of a\n  - nested\n- b")).toEqual([
			list(false, 1, [text("a\nmore of a"), bullets("nested")], [text("b")]),
		]);
		expect(splitMessageLists("1. step\n   1. sub one\n   2. sub two\n2. next")).toEqual([
			list(true, 1, [text("step"), list(true, 1, [text("sub one")], [text("sub two")])], [text("next")]),
		]);
	});

	it("stops nesting at three levels and shows deeper markers as typed", () => {
		const blocks = splitMessageLists("- a\n  - b\n    - c\n      - d");
		const top = blocks[0];
		if (top?.kind !== "list") throw new Error("expected a list");
		const second = top.items[0]?.[1];
		if (second?.kind !== "list") throw new Error("expected a nested list");
		const third = second.items[0]?.[1];
		if (third?.kind !== "list") throw new Error("expected a third-level list");
		expect(third.items[0]).toEqual([text("c\n- d")]);
	});

	it("does not treat anything inside a code fence as a list", () => {
		expect(splitMessageLists("Run:\n```\n- not a list\n1. nor this\n```\n- real")).toEqual([
			text("Run:\n```\n- not a list\n1. nor this\n```"),
			bullets("real"),
		]);
	});

	it("keeps markup characters in items as plain text for React to escape", () => {
		expect(splitMessageLists("- <script>alert(1)</script>\n- **b** `c` [d](javascript:x)")).toEqual([
			bullets("<script>alert(1)</script>", "**b** `c` [d](javascript:x)"),
		]);
	});

	it("never loses or reorders the words of a message", () => {
		const sample = "intro\n- a\n  cont\n\n1. b\nplain\n```\n- x\n```\n- c\n  - nested";
		const words = (value: string) => value.split(/\s+/).filter((word) => word !== "" && !/^(?:[-*+]|\d+[.)])$/.test(word));
		const flatten = (blocks: MessageBlock[]): string[] =>
			blocks.flatMap((block) => (block.kind === "text" ? [block.text] : block.items.flatMap(flatten)));
		expect(words(flatten(splitMessageLists(sample)).join("\n"))).toEqual(words(sample));
	});
});

describe("listContinuation", () => {
	it("continues a bullet with the same marker and spacing", () => {
		expect(listContinuation("- one", "")).toEqual({ kind: "continue", prefix: "- " });
		expect(listContinuation("  * one", "")).toEqual({ kind: "continue", prefix: "  * " });
		expect(listContinuation("+   wide", "")).toEqual({ kind: "continue", prefix: "+   " });
	});

	it("increments a number and keeps its delimiter", () => {
		expect(listContinuation("1. a", "")).toEqual({ kind: "continue", prefix: "2. " });
		expect(listContinuation("9) a", "")).toEqual({ kind: "continue", prefix: "10) " });
	});

	it("ends the list on an empty item", () => {
		expect(listContinuation("- ", "")).toEqual({ kind: "end" });
		expect(listContinuation("3.  ", "")).toEqual({ kind: "end" });
	});

	it("does nothing for prose, a mid-item caret or a marker without a space", () => {
		expect(listContinuation("just words", "")).toBeNull();
		expect(listContinuation("- ab", "c")).toBeNull();
		expect(listContinuation("-nospace", "")).toBeNull();
		expect(listContinuation("    - too indented", "")).toBeNull();
	});
});
