// Non-web stub for the visibility-change hook. There is no document on
// desktop/mobile; the lifecycle observer (WidgetsBindingObserver) covers
// resume there, so this is intentionally a no-op.
void registerVisibilityListener(void Function() onVisible) {
  // no-op on non-web platforms
}
