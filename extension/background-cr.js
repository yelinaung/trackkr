// Chrome entrypoint: an MV3 service worker.
//
// importScripts is synchronous and deliberately so. A worker is woken by an
// event, and Chrome delivers that event only if the listener already exists
// when global evaluation finishes. Loading these as ES modules, or awaiting
// anything before registerListeners, would drop the very event that started
// this worker.
importScripts("logic.js", "common.js", "background-core.js");

registerListeners();

// No runtime.onSuspend here. Chrome does not provide it -- the worker is killed
// without warning after roughly thirty seconds idle -- so there is no hook to
// flush from and nothing may depend on one existing. Durability comes from
// finalization writing the queue before clearing the session segment, and from
// recovery reconciling whatever a kill left behind.

// Second phase: storage reads and delivery, after registration is complete.
recover();
