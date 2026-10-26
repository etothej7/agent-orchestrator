import { describe, expect, it } from "vitest";
import { coordinatorBadgeCount } from "./coordinator-badge";

const note = (type: string, purpose: string | undefined, status: string) => ({ type, purpose, status });

describe("coordinatorBadgeCount", () => {
	it("is the daemon's open-question count plus unread results and stops", () => {
		expect(coordinatorBadgeCount({
			unresolvedAskCount: 2,
			notifications: [
				note("coordinator_message", "verified_result", "unread"),
				note("coordinator_message", "stopped_work", "unread"),
				note("coordinator_message", "stopped_work", "read"),
				note("coordinator_message", "blocking_decision", "unread"),
				note("needs_input", undefined, "unread"),
			],
		})).toBe(4);
	});

	it("takes the open count from the daemon, not from the page it was handed", () => {
		expect(coordinatorBadgeCount({ unresolvedAskCount: 5, notifications: [] })).toBe(5);
		expect(coordinatorBadgeCount({ notifications: [] })).toBe(0);
	});

	it("an ask does not count twice and reading a tell clears it", () => {
		expect(coordinatorBadgeCount({ unresolvedAskCount: 1, notifications: [note("coordinator_message", "scope_question", "unread")] })).toBe(1);
		expect(coordinatorBadgeCount({ unresolvedAskCount: 0, notifications: [note("coordinator_message", "verified_result", "read")] })).toBe(0);
	});
});
