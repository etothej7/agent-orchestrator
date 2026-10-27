import { useCallback, useEffect, useRef, useState, type RefObject } from "react";

const MAX_TRIES = 20;
const MAX_PAGES = 60;
const HIGHLIGHT_MS = 1800;

/**
 * Brings one coordinator block into view and marks it for a moment. The transcript may be virtualized or
 * have the block on an older page, so it keeps nudging (scroll to its row, load earlier) until the block
 * is in the DOM, then centers it.
 */
export function useJumpToCoordinatorMessage({
	root,
	indexOf,
	scrollToIndex,
	hasOlder,
	loadingOlder,
	onLoadOlder,
	stopFollow,
}: {
	root: RefObject<HTMLElement | null>;
	/** The row holding the message, or -1 when it is not loaded. */
	indexOf: (messageId: string) => number;
	scrollToIndex?: (index: number) => void;
	hasOlder?: boolean;
	loadingOlder?: boolean;
	onLoadOlder?: () => void;
	/** Stop the transcript following new output so it does not pull the reader back down. */
	stopFollow: () => void;
}): (messageId: string, onReached?: () => void, onEnd?: () => void) => void {
	const [target, setTarget] = useState<{ id: string; tries: number; pages: number; onReached?: () => void; onEnd?: () => void }>();
	// The inputs churn every render; only a new target or retry should run the search.
	const latest = useRef({ indexOf, scrollToIndex, hasOlder, loadingOlder, onLoadOlder, stopFollow });
	latest.current = { indexOf, scrollToIndex, hasOlder, loadingOlder, onLoadOlder, stopFollow };
	const highlight = useRef<{ element: HTMLElement; timer: number }>(undefined);
	const clearHighlight = useCallback(() => {
		if (!highlight.current) return;
		window.clearTimeout(highlight.current.timer);
		highlight.current.element.classList.remove("ring-2", "ring-ring");
		highlight.current = undefined;
	}, []);
	useEffect(() => clearHighlight, [clearHighlight]);
	useEffect(() => {
		if (!target) return;
		// The jump is over: reached the block, or gave up (nothing to scroll to, no older page, out of retries).
		const end = (reached: boolean) => {
			setTarget(undefined);
			if (reached) target.onReached?.();
			target.onEnd?.();
		};
		const element = Array.from(root.current?.querySelectorAll<HTMLElement>("[data-coordinator-message-id]") ?? [])
			.find((candidate) => candidate.dataset.coordinatorMessageId === target.id);
		if (element) {
			latest.current.stopFollow();
			element.scrollIntoView?.({ block: "center" });
			// The transcript may still be settling (opening at the bottom); a block that is not
			// in view afterwards means it was moved again, so nudge it once more.
			const viewport = element.closest("[role=log]")?.getBoundingClientRect();
			const rect = element.getBoundingClientRect();
			// A viewport without layout (no rendering) cannot tell, so the block counts as reached.
			const inView = !viewport || viewport.height === 0 || (rect.bottom > viewport.top && rect.top < viewport.bottom);
			if (inView || target.tries >= MAX_TRIES) {
				clearHighlight();
				element.classList.add("ring-2", "ring-ring");
				highlight.current = { element, timer: window.setTimeout(clearHighlight, HIGHLIGHT_MS) };
				end(true);
				return;
			}
			const settle = window.setTimeout(() => setTarget((current) => current && { ...current, tries: current.tries + 1 }), 120);
			return () => window.clearTimeout(settle);
		}
		const { indexOf: find, scrollToIndex: scrollTo, hasOlder: older, loadingOlder: loading, onLoadOlder: loadOlder } = latest.current;
		const index = find(target.id);
		let pages = target.pages;
		if (index >= 0) scrollTo?.(index);
		else if (older && !loading) {
			// Page back until the message is loaded. Waiting for a page does not use up the retries.
			if (pages >= MAX_PAGES) {
				end(false);
				return;
			}
			pages += 1;
			loadOlder?.();
		} else if (!older) {
			end(false);
			return;
		}
		const waitingForPage = index < 0 && loading;
		if (!waitingForPage && target.tries >= MAX_TRIES) {
			end(false);
			return;
		}
		const retry = window.setTimeout(
			() => setTarget((current) => current && { ...current, pages, tries: waitingForPage || index < 0 ? current.tries : current.tries + 1 }),
			80,
		);
		return () => window.clearTimeout(retry);
	}, [clearHighlight, root, target]);
	return useCallback((messageId: string, onReached?: () => void, onEnd?: () => void) => setTarget({ id: messageId, tries: 0, pages: 0, onReached, onEnd }), []);
}
