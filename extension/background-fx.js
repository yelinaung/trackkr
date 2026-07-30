// Firefox entrypoint.
//
// The manifest loads logic.js, common.js, and background-core.js ahead of this
// file as ordered classic scripts, so everything is already in scope. All that
// remains is the lifecycle Firefox provides and Chrome does not.

registerListeners();

// Firefox unloads an idle event page and fires this, which is not the same as
// the browser exiting. Finalizing here would truncate an ongoing visit and stop
// tracking until some later event happened to wake the page again -- so the
// current segment is deliberately left alone. It lives in session storage and
// survives the unload.
//
// Delivery is best effort: asynchronous work is not guaranteed to finish during
// suspension, which is precisely why the queue is made durable before this
// point rather than during it.
api.runtime.onSuspend.addListener(() => serialize(deliver));

// Recovery is a second phase, after every listener is installed. It reads
// storage, so it cannot run before registration without risking the event that
// woke this page.
recover();
