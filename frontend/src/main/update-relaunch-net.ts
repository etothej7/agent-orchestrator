import { execFile, spawn, type ChildProcess, type SpawnOptions } from "node:child_process";
import { lstat, mkdir, readFile, rm, stat, writeFile } from "node:fs/promises";
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
//   outcome   what the net did, written once, then it exits: `cancelled`, `app_running`, `relaunched`, `recovery_ok`,
//             `recovery_relaunched`, `recovery_incomplete`, `recovery_failed` or `blocked_marker`
//
// A swap marker with no live installer (no live lock owner) means the app bundle may be half-swapped; so does one that is still there when
// the wait for a live installer runs out. The net then
// NEVER relaunches (launching it would block the installer's own startup recovery, which refuses while the app or
// daemon runs with a marker). It runs the installer's recovery entry if one is recorded (the request's
// `recoveryEntry`, or `M_RECOVERY="<path>"` in the marker), which restores a consistent bundle and relaunches; if
// there is none, or it fails, AO stays closed and the net writes a stopped result (`update/runs/<stamp>/result.json`,
// write-once, a check named `relaunch_net_blocked_marker` / `relaunch_net_recovery_failed`) for the update manager.

/** How long the net waits before it looks. */
export const RELAUNCH_NET_DELAY_SECONDS = 600;
/** How long it keeps waiting for an install that is mid-swap or rolling back before it gives up waiting. */
export const RELAUNCH_NET_LOCK_WAIT_SECONDS = 45 * 60;
/** After a recovery entry succeeds, how long the net waits for AO to come back before relaunching it itself. */
export const RELAUNCH_NET_RECOVERY_WAIT_SECONDS = 120;
const STEP_SECONDS = 2;
/** The marker the net process carries in its command line (argv[0] of the shell), so a pid can be told to be a net. */
export const RELAUNCH_NET_PROCESS_NAME = "ao-relaunch-net";

export const relaunchNetDir = (stateDir: string, stamp: string): string => path.join(stateDir, "update-restart", "relaunch-net", stamp);

/** The anchored extended regex `pgrep -f` uses to find this app's main process, with the path escaped. */
export const appProcessPattern = (appPath: string): string => `^${appPath.replace(/[\\^$.*+?()[\]{}|]/g, "\\$&")}/Contents/MacOS/`;

// POSIX sh. Arguments: app dir delaySteps step lockSteps lock marker open pgrep pattern recovery stamp from to resultFile.
// The commands are arguments so the tests can substitute fakes; production passes /usr/bin/open and /usr/bin/pgrep.
// Waits poll the cancel file every step. Nothing from the arguments is interpolated into the script text.
const NET_SCRIPT = `
umask 077
app=$1; dir=$2; delaysteps=$3; step=$4; locksteps=$5; lock=$6; marker=$7; opencmd=$8; pgrepcmd=$9
pattern=\${10}; recovery=\${11}; stamp=\${12}; from=\${13}; to=\${14}; resultfile=\${15}; recoverysteps=\${16}
printf '%s\\n' "$$" > "$dir/net.pid.tmp" && mv -f "$dir/net.pid.tmp" "$dir/net.pid" || exit 1
finish() { printf '%s\\n' "$1" > "$dir/outcome"; exit 0; }
cancelled() { [ -e "$dir/cancel" ]; }
write_result() { # write_result <check name> <text>: a stopped result for the update manager; never overwrites one
  mkdir -p "$(dirname "$resultfile")" 2>/dev/null || return 0
  now=$(( $(date +%s) * 1000 ))
  tmp="$resultfile.net.$$"
  printf '{"schemaVersion":1,"stamp":"%s","outcome":"stopped","fromVersion":"%s","toVersion":"%s","text":"%s","checks":[{"name":"%s","result":"fail"}],"at":%s}\\n' "$stamp" "$from" "$to" "$2" "$1" "$now" > "$tmp" && ln "$tmp" "$resultfile" 2>/dev/null
  rm -f "$tmp"
}
i=0
while [ "$i" -lt "$delaysteps" ]; do
  cancelled && finish cancelled
  sleep "$step"; i=$((i+1))
done
j=0
while :; do
  cancelled && finish cancelled
  "$pgrepcmd" -a -f "$pattern" >/dev/null 2>&1 && finish app_running   # -a: pgrep skips the caller's ANCESTORS (the app that spawned this net) unless asked
  # Only a LIVE installer is waited for. The installer holds its lock for its whole run and removes it last, so a swap marker with no live
  # lock owner means the installer died mid-swap: nothing will clear it by waiting, recovery can start at once.
  busy=0
  if [ -d "$lock" ]; then
    lockpid=$(cat "$lock/pid" 2>/dev/null)
    case "$lockpid" in
      '') busy=1 ;;                                  # the lock was just taken: its owner has not written its pid yet
      *[!0-9]*) ;;                                   # not a pid (a sign, a space, text): a stale lock, ignored
      *) kill -0 "$lockpid" 2>/dev/null && busy=1 ;;
    esac
  fi
  [ "$busy" -eq 0 ] && break
  [ "$j" -ge "$locksteps" ] && break
  sleep "$step"; j=$((j+1))
done
cancelled && finish cancelled
if [ -e "$marker" ]; then
  # Possibly half-swapped: never launch it. Hand over to the installer's recovery, or stay closed.
  rec=$recovery
  [ -n "$rec" ] || rec=$(sed -n 's/^M_RECOVERY="\\(.*\\)"$/\\1/p' "$marker" 2>/dev/null | head -n 1)
  # a recovery entry is run only if it is an absolute path to a regular, executable file owned by this user and not group/world-writable
  # (the request's entry was already checked in main; a marker-supplied one is only checked here)
  case "$rec" in
    /*)
      ok=0
      if [ -f "$rec" ] && [ -x "$rec" ]; then
        st=$(stat -f '%u %Lp' "$rec" 2>/dev/null); ru=\${st%% *}; rmode=\${st#* }
        if [ "$ru" = "$(id -u)" ] && [ $(( 0$rmode & 18 )) -eq 0 ]; then ok=1; fi
      fi
      [ "$ok" -eq 1 ] || rec="" ;;
    *) rec="" ;;
  esac
  if [ -n "$rec" ]; then
    printf '%s\\n' recovery_invoked > "$dir/outcome"
    if "$rec" >/dev/null 2>&1; then
      # A recovery that restores a consistent bundle should bring AO back itself. Check, rather than assume.
      k=0
      while [ "$k" -lt "$recoverysteps" ]; do
        cancelled && finish cancelled
        "$pgrepcmd" -a -f "$pattern" >/dev/null 2>&1 && finish recovery_ok
        sleep "$step"; k=$((k+1))
      done
      if [ ! -e "$marker" ]; then printf '%s\\n' recovery_relaunched > "$dir/outcome"; exec "$opencmd" -g -j "$app"; fi
      write_result relaunch_net_recovery_incomplete "AO was left closed: the installer recovery finished but an interrupted-update marker is still there. Run the recovery again before starting AO."
      finish recovery_incomplete
    fi
    write_result relaunch_net_recovery_failed "AO was left closed: the installer recovery after an interrupted update did not finish. Run the recovery again before starting AO."
    finish recovery_failed
  fi
  write_result relaunch_net_blocked_marker "AO was left closed on purpose: an update was interrupted part-way and the installer has to recover it before AO can start. Nothing was relaunched."
  finish blocked_marker
fi
printf '%s\\n' relaunched > "$dir/outcome"
exec "$opencmd" -g -j "$app"
`;

export type SpawnNet = (command: string, args: string[], options: SpawnOptions) => ChildProcess;

export type RelaunchNetOptions = {
	stateDir: string;
	/** The installed app bundle to relaunch. */
	appPath: string;
	/** The version running now (the result the net may write names it as the version before the update). */
	fromVersion: string;
	/** The installer's lock folder (`~/.ao/install.lock`, a `pid` file inside) and swap/rollback marker. */
	lockPath: string;
	markerPath: string;
	/** The user id that must own a recovery entry. */
	uid?: number;
	delaySeconds?: number;
	lockWaitSeconds?: number;
	/** How long to wait, after a recovery entry succeeds, for AO to be running again before the net relaunches it itself. */
	recoveryWaitSeconds?: number;
	stepSeconds?: number;
	openCommand?: string;
	pgrepCommand?: string;
	spawnNet?: SpawnNet;
	/** Whether the pid is a live relaunch-net process for this folder. Defaults to asking `ps`. */
	isNetProcess?: (pid: number, dir: string) => Promise<boolean>;
};

export type RelaunchNetRequest = {
	stamp: string;
	/** The version being installed. */
	version: string;
	/** The installer's recovery entry (an absolute path), tried when a swap marker is left behind. */
	recoveryEntry?: string;
};

const exists = (file: string) => stat(file).then(() => true, () => false);
const SAFE_VERSION = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;

/** The process is a live relaunch net started for this folder (a reused pid of another program is not). */
function psIsNet(pid: number, dir: string): Promise<boolean> {
	return new Promise((resolve) => {
		execFile("/bin/ps", ["-ww", "-p", String(pid), "-o", "command="], { timeout: 5_000 }, (error, stdout) => {
			resolve(!error && stdout.includes(` ${RELAUNCH_NET_PROCESS_NAME} `) && stdout.includes(dir));
		});
	});
}

export function createRelaunchNet(options: RelaunchNetOptions) {
	const step = options.stepSeconds ?? STEP_SECONDS;
	const delaySteps = Math.max(1, Math.ceil((options.delaySeconds ?? RELAUNCH_NET_DELAY_SECONDS) / step));
	const lockSteps = Math.max(0, Math.ceil((options.lockWaitSeconds ?? RELAUNCH_NET_LOCK_WAIT_SECONDS) / step));
	const recoverySteps = Math.max(1, Math.ceil((options.recoveryWaitSeconds ?? RELAUNCH_NET_RECOVERY_WAIT_SECONDS) / step));
	const spawnNet: SpawnNet = options.spawnNet ?? ((command, args, spawnOptions) => spawn(command, args, spawnOptions));
	const isNetProcess = options.isNetProcess ?? psIsNet;
	const uid = options.uid ?? process.getuid?.() ?? -1;

	/** A recovery entry the net may run: absolute, a regular file, owned by the user, not group/world-writable, executable. */
	const usableRecovery = async (entry: string | undefined): Promise<string> => {
		if (!entry || !path.isAbsolute(entry) || entry.includes("\0") || entry.length > 1024) return "";
		try {
			const info = await lstat(entry);
			return info.isFile() && info.uid === uid && (info.mode & 0o022) === 0 && (info.mode & 0o100) !== 0 ? entry : "";
		} catch {
			return "";
		}
	};

	return {
		/** Arms the net for this update. True only once the detached process is running; idempotent per stamp. */
		async arm(request: RelaunchNetRequest): Promise<boolean> {
			const dir = relaunchNetDir(options.stateDir, request.stamp);
			try {
				await mkdir(dir, { recursive: true, mode: 0o700 });
				const pid = Number((await readFile(path.join(dir, "net.pid"), "utf8").catch(() => "")).trim());
				if (pid > 1 && !(await exists(path.join(dir, "outcome"))) && (await isNetProcess(pid, dir))) {
					await rm(path.join(dir, "cancel"), { force: true });
					return true;
				}
				await Promise.all(["cancel", "outcome", "net.pid"].map((name) => rm(path.join(dir, name), { force: true })));
				const from = SAFE_VERSION.test(options.fromVersion) ? options.fromVersion : "0.0.0";
				const to = SAFE_VERSION.test(request.version) ? request.version : "0.0.0";
				const child = spawnNet(
					"/bin/sh",
					[
						"-c", NET_SCRIPT, RELAUNCH_NET_PROCESS_NAME,
						options.appPath, dir, String(delaySteps), String(step), String(lockSteps),
						options.lockPath, options.markerPath, options.openCommand ?? "/usr/bin/open", options.pgrepCommand ?? "/usr/bin/pgrep",
						appProcessPattern(options.appPath), await usableRecovery(request.recoveryEntry),
						request.stamp, from, to, path.join(options.stateDir, "update", "runs", request.stamp, "result.json"), String(recoverySteps),
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
