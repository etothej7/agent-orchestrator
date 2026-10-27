import { spawn, type ChildProcess, type SpawnOptions } from "node:child_process";
import { mkdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

// The relaunch safety net. Before main honours an update quit it arms a recovery that does not depend on the update
// runner staying alive: a detached process in its own session that, after a delay, relaunches AO hidden
// (`open -g -j`) if AO is not running and no install is mid-swap or rolling back. Whatever dies (the runner, the
// update manager, the new app), AO is never closed for longer than the delay. The runner cancels the net on success
// by creating `cancel` in the net's folder. See frontend/docs/unattended-update.md.
//
// Files, under `<stateDir>/update-restart/relaunch-net/<stamp>/` (folder 0700):
//   net.pid   written by the net process as soon as it runs (proof that it is armed)
//   cancel    created by the runner (or by main when the quit is called off) to stop the net
//   outcome   `cancelled`, `app_running` or `relaunched`: what the net did, written once, then it exits

/** How long the net waits before it looks. */
export const RELAUNCH_NET_DELAY_SECONDS = 600;
/** How long it keeps waiting for an install that is mid-swap or rolling back before it relaunches anyway. */
export const RELAUNCH_NET_LOCK_WAIT_SECONDS = 45 * 60;
const STEP_SECONDS = 2;

export const relaunchNetDir = (stateDir: string, stamp: string): string => path.join(stateDir, "update-restart", "relaunch-net", stamp);

// POSIX sh. Arguments: app exe dir delaySteps step lockSteps lock marker open pgrep. The commands are arguments so the
// tests can substitute fakes; production passes /usr/bin/open and /usr/bin/pgrep. Waits poll the cancel file every step.
const NET_SCRIPT = `
app=$1; exe=$2; dir=$3; delaysteps=$4; step=$5; locksteps=$6; lock=$7; marker=$8; opencmd=$9; pgrepcmd=\${10}
printf '%s\\n' "$$" > "$dir/net.pid.tmp" && mv -f "$dir/net.pid.tmp" "$dir/net.pid" || exit 1
finish() { printf '%s\\n' "$1" > "$dir/outcome"; exit 0; }
cancelled() { [ -e "$dir/cancel" ]; }
i=0
while [ "$i" -lt "$delaysteps" ]; do
  cancelled && finish cancelled
  sleep "$step"; i=$((i+1))
done
j=0
while :; do
  cancelled && finish cancelled
  "$pgrepcmd" -f "^$app/Contents/MacOS/" >/dev/null 2>&1 && finish app_running
  busy=0
  [ -e "$marker" ] && busy=1
  if [ -d "$lock" ]; then
    lockpid=$(cat "$lock/pid" 2>/dev/null)
    if [ -z "$lockpid" ] || kill -0 "$lockpid" 2>/dev/null; then busy=1; fi
  fi
  [ "$busy" -eq 0 ] && break
  [ "$j" -ge "$locksteps" ] && break
  sleep "$step"; j=$((j+1))
done
cancelled && finish cancelled
printf '%s\\n' relaunched > "$dir/outcome"
exec "$opencmd" -g -j "$app"
`;

export type SpawnNet = (command: string, args: string[], options: SpawnOptions) => ChildProcess;

export type RelaunchNetOptions = {
	stateDir: string;
	/** The installed app bundle to relaunch. */
	appPath: string;
	/** The name of the app's main executable (to tell whether AO is running). */
	exeName: string;
	/** The installer's lock folder (`~/.ao/install.lock`, a `pid` file inside) and swap/rollback marker. */
	lockPath: string;
	markerPath: string;
	delaySeconds?: number;
	lockWaitSeconds?: number;
	stepSeconds?: number;
	openCommand?: string;
	pgrepCommand?: string;
	spawnNet?: SpawnNet;
};

const exists = (file: string) => stat(file).then(() => true, () => false);

export function createRelaunchNet(options: RelaunchNetOptions) {
	const step = options.stepSeconds ?? STEP_SECONDS;
	const delaySteps = Math.max(1, Math.ceil((options.delaySeconds ?? RELAUNCH_NET_DELAY_SECONDS) / step));
	const lockSteps = Math.max(0, Math.ceil((options.lockWaitSeconds ?? RELAUNCH_NET_LOCK_WAIT_SECONDS) / step));
	const spawnNet: SpawnNet = options.spawnNet ?? ((command, args, spawnOptions) => spawn(command, args, spawnOptions));

	const alive = (pid: number) => {
		try {
			process.kill(pid, 0);
			return true;
		} catch {
			return false;
		}
	};

	return {
		/** Arms the net for this update. True only once the detached process is running; idempotent per stamp. */
		async arm(stamp: string): Promise<boolean> {
			const dir = relaunchNetDir(options.stateDir, stamp);
			try {
				await mkdir(dir, { recursive: true, mode: 0o700 });
				const pid = Number((await readFile(path.join(dir, "net.pid"), "utf8").catch(() => "")).trim());
				if (pid > 1 && alive(pid) && !(await exists(path.join(dir, "outcome")))) {
					await rm(path.join(dir, "cancel"), { force: true });
					return true;
				}
				await Promise.all(["cancel", "outcome", "net.pid"].map((name) => rm(path.join(dir, name), { force: true })));
				const child = spawnNet(
					"/bin/sh",
					[
						"-c", NET_SCRIPT, "ao-relaunch-net",
						options.appPath, options.exeName, dir, String(delaySteps), String(step), String(lockSteps),
						options.lockPath, options.markerPath, options.openCommand ?? "/usr/bin/open", options.pgrepCommand ?? "/usr/bin/pgrep",
					],
					{
						detached: true, // its own session: it outlives this app
						stdio: "ignore",
						cwd: "/",
						env: { PATH: "/usr/bin:/bin:/usr/sbin:/sbin", HOME: os.homedir() },
					},
				);
				await new Promise<void>((resolve, reject) => {
					child.once("spawn", () => resolve());
					child.once("error", reject);
				});
				child.unref();
				for (let waited = 0; waited < 3_000; waited += 20) {
					if (await exists(path.join(dir, "net.pid"))) return true;
					await new Promise((resolve) => setTimeout(resolve, 20));
				}
				return false;
			} catch {
				return false;
			}
		},

		/** Stops the net (the runner's success path, or main when the quit is called off). A no-op for an unarmed stamp. */
		async cancel(stamp: string): Promise<void> {
			const dir = relaunchNetDir(options.stateDir, stamp);
			if (!(await exists(dir))) return;
			await writeFile(path.join(dir, "cancel"), "", { mode: 0o600 }).catch(() => undefined);
		},
	};
}
