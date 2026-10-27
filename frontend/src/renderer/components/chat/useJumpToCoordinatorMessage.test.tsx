import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { useJumpToCoordinatorMessage } from "./useJumpToCoordinatorMessage";

function setup(overrides: { hasOlder?: boolean; present?: string[] } = {}) {
	const root = document.createElement("div");
	for (const id of overrides.present ?? []) {
		const block = document.createElement("section");
		block.dataset.coordinatorMessageId = id;
		root.appendChild(block);
	}
	document.body.appendChild(root);
	const onLoadOlder = vi.fn();
	const hook = renderHook(() =>
		useJumpToCoordinatorMessage({
			root: { current: root },
			indexOf: (id) => ((overrides.present ?? []).includes(id) ? 0 : -1),
			hasOlder: overrides.hasOlder,
			loadingOlder: false,
			onLoadOlder,
			stopFollow: () => undefined,
		}),
	);
	return { jump: hook.result, onLoadOlder, cleanup: () => root.remove() };
}

describe("useJumpToCoordinatorMessage", () => {
	it("tells the caller a message that is not loaded and cannot be loaded is missing", async () => {
		const { jump, cleanup } = setup({ hasOlder: false });
		const onEnd = vi.fn();
		act(() => jump.current("absent", { onEnd }));
		await waitFor(() => expect(onEnd).toHaveBeenCalledWith({ reached: false, missing: true }));
		cleanup();
	});

	it("reaches a loaded message and reports it", async () => {
		const { jump, cleanup } = setup({ present: ["here"] });
		const onReached = vi.fn();
		const onEnd = vi.fn();
		act(() => jump.current("here", { onReached, onEnd }));
		await waitFor(() => expect(onReached).toHaveBeenCalled());
		expect(onEnd).toHaveBeenCalledWith({ reached: true, missing: false });
		cleanup();
	});

	it("lets a jump that is replaced end, so nothing waits on it forever", async () => {
		const { jump, onLoadOlder, cleanup } = setup({ hasOlder: true, present: ["second"] });
		const firstEnd = vi.fn();
		act(() => jump.current("first", { onEnd: firstEnd })); // pages back for a message that never loads
		await waitFor(() => expect(onLoadOlder).toHaveBeenCalled());
		const secondReached = vi.fn();
		act(() => jump.current("second", { onReached: secondReached }));
		expect(firstEnd).toHaveBeenCalledWith({ reached: false, missing: false, replaced: true });
		await waitFor(() => expect(secondReached).toHaveBeenCalled());
		cleanup();
	});
});
