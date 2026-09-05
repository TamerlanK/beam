const LOCK = "beam:device";
const STEAL_AFTER = 1500;

let hooks = null;
let chan = null;
let leader = false;
let release = null;
let stealing = false;
let yielding = false;
let waitCtl = null;
let stealTimer = 0;

export const supported = !!(navigator.locks && "BroadcastChannel" in window);

export function initTabs(h) {
  hooks = h;
  if (!supported) {
    hooks.onLead();
    return;
  }
  chan = new BroadcastChannel("beam:tabs");
  chan.onmessage = ({ data }) => {
    if (data === "takeover" && leader) {
      if (hooks.isBusy()) chan.postMessage("busy");
      else yieldLead();
    } else if (data === "busy" && !leader) {
      clearTimeout(stealTimer);
      stealTimer = 0;
      hooks.onWait("busy");
    }
  };
  run({ ifAvailable: true });
}

export const isLeader = () => leader;

export function takeover() {
  if (!supported || leader || stealing || yielding) return;
  chan.postMessage("takeover");
  hooks.onWait("asking");
  clearTimeout(stealTimer);
  stealTimer = setTimeout(() => {
    stealTimer = 0;
    if (leader || stealing) return;
    stealing = true;
    run({ steal: true });
  }, STEAL_AFTER);
}

async function yieldLead() {
  if (yielding || !leader) return;
  yielding = true;
  try {
    await hooks.onYield();
  } catch {}
  yielding = false;
  if (release) release();
}

async function run(opts) {
  let held = false;
  try {
    held = await navigator.locks.request(LOCK, opts, async (lock) => {
      if (!lock) return false;
      if (opts.steal) {
        stealing = false;
        if (waitCtl) waitCtl.abort();
      }
      if (!leader) {
        leader = true;
        clearTimeout(stealTimer);
        stealTimer = 0;
        hooks.onLead();
      }
      await new Promise((r) => {
        release = r;
      });
      leader = false;
      release = null;
      return true;
    });
  } catch {
    if (opts.signal && opts.signal.aborted) return;
    if (opts.steal) {
      stealing = false;
      return;
    }
    if (leader && !stealing) {
      leader = false;
      release = null;
      hooks.onYield();
    } else if (leader) {
      return;
    }
  }
  if (leader) return;
  waitCtl = new AbortController();
  hooks.onWait(held ? "yielded" : "held");
  run({ signal: waitCtl.signal });
}

export const debug = {
  get chan() {
    return chan;
  },
};
