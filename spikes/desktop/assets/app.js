import { Call, Events } from "/wails/runtime.js";

const call = (name, ...args) => Call.ByName(`main.SpikeService.${name}`, ...args);
const status = document.querySelector("#status");
const burst = document.querySelector("#burst");
const slow = document.querySelector("#slow");
const cancel = document.querySelector("#cancel");
let runId = "";
let received = 0;
let batches = 0;
let sequenceError = false;
let starting = null;

Events.On("spike:batch", async ({ data }) => {
  // Register before Start so the first batch cannot be missed.
  // Wait for the run identity before accepting any queued events from an old job.
  if (starting) await starting.catch(() => {});
  if (data.runId !== runId) return;
  batches++;
  for (const event of data.events) {
    if (event.sequence !== received + 1) sequenceError = true;
    received++;
  }
  status.textContent = `Получено: ${received}; пакетов: ${batches}; порядок: ${sequenceError ? "ОШИБКА" : "OK"}`;
  try {
    await call("Ack", data.runId, data.events.at(-1).sequence);
  } catch (error) {
    status.textContent = String(error);
  }
});

async function start(delay) {
  received = batches = 0;
  sequenceError = false;
  burst.disabled = slow.disabled = true;
  try {
    starting = call("Start", 10000, delay).then(id => { runId = id; });
    await starting;
    cancel.disabled = false;
  } catch (error) {
    status.textContent = String(error);
    burst.disabled = slow.disabled = false;
  } finally {
    starting = null;
  }
}
burst.onclick = () => start(0);
slow.onclick = () => start(10);
cancel.onclick = () => call("Cancel", runId).catch(error => { status.textContent = String(error); });

async function refresh() {
  try {
    const job = await call("Status");
    const running = job.state === "running";
    burst.disabled = slow.disabled = running;
    cancel.disabled = !running;
    if (!running) status.textContent = `${job.state}; подтверждено Go: ${job.delivered}; получено UI: ${received}; пакетов: ${batches}; порядок: ${sequenceError ? "ОШИБКА" : "OK"}`;
  } catch (error) {
    status.textContent = String(error);
  }
}
await refresh();
// One pending bridge call at a time even if the UI is slow.
async function poll() { await refresh(); setTimeout(poll, 250); }
setTimeout(poll, 250);
