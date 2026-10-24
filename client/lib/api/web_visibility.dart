// Web implementation of the visibility-change hook.
// Imported on web builds only (see the conditional import in main.dart).
import 'dart:js_interop';

import 'package:web/web.dart' as web;

/// Registers a document-level visibilitychange listener. When the browser
/// tab becomes visible again (user clicks back into it after Firefox
/// throttled the background tab and possibly dropped SSE sockets),
/// [onVisible] is invoked so the app can force-reconnect stale streams.
void registerVisibilityListener(void Function() onVisible) {
  web.document.addEventListener('visibilitychange', (web.Event event) {
    if (web.document.visibilityState == 'visible') {
      onVisible();
    }
  }.toJS);
}
