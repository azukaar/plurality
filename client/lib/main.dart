import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:app_links/app_links.dart';
import 'package:plurality/api/MCP.dart';
import 'package:plurality/api/skills_service.dart';
import 'package:plurality/api/chat_service.dart';
import 'package:plurality/api/client_identity.dart';
import './auth/auth-service.dart';
import './auth/account.dart';
import './auth/login.dart';
import './api/storage.dart';
import './utils/index.dart';
import './utils/deep_link.dart';
import './api/preferences_provider.dart';
import 'chat/index.dart';
import 'dart:async';
import 'dart:io' show Platform;
import 'package:flutter/rendering.dart';

// Conditional import: the web build registers a document-level
// visibilitychange listener (to recover SSE sockets that Firefox dropped
// while the tab was backgrounded); desktop/mobile get a no-op stub and
// rely on the WidgetsBindingObserver lifecycle instead.
import 'package:plurality/api/web_visibility.dart'
    if (dart.library.io) 'package:plurality/api/web_visibility_stub.dart';

final authStateProvider = StreamProvider<User?>((ref) {
  return AuthService().authStateChanges;
});

void main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await AuthService.loadServerUrl();
  await ConversationStorage.init();
  await ClientIdentity().init();
  debugPaintSizeEnabled = false;

  checkVersion();

  MCPService().initMCP();
  SkillsService().initSkills();

  runApp(ProviderScope(child: MyApp()));
}

final c = Colors.red;

final ThemeData lightTheme = ThemeData(
  colorScheme: ColorScheme.fromSeed(seedColor: c),
);

final ThemeData darkTheme = ThemeData(
  colorScheme: ColorScheme.fromSeed(seedColor: c, brightness: Brightness.dark),
);

class MyApp extends ConsumerStatefulWidget {
  const MyApp({super.key});

  static bool newVersionAvailable = false;

  @override
  ConsumerState<MyApp> createState() => _MyAppState();
}

class _MyAppState extends ConsumerState<MyApp> with WidgetsBindingObserver {
  StreamSubscription<Uri>? _linkSub;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addObserver(this);
    _initDeepLinks();
    _initVisibilityListener();
  }

  /// Firefox (and other browsers) throttle background tabs and can drop
  /// long-lived SSE sockets without firing onError/onDone. When the user
  /// clicks back into the tab, the document becomes visible again — hook
  /// that event to force-reconnect any stuck session and refresh the
  /// global status stream. Without this the chat can sit frozen on
  /// "processing" until a manual tab reload.
  void _initVisibilityListener() {
    registerVisibilityListener(() {
      // Only reconnect when authenticated — the ChatService itself also
      // guards on _connected, but avoid the churn of a reconnectVisible()
      // against a logged-out / signed-out session.
      if (ref.read(authStateProvider).valueOrNull != null) {
        ChatService().reconnectVisible();
      }
    });
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (state == AppLifecycleState.resumed) {
      // On mobile the OS may have severed SSE sockets while backgrounded,
      // leaving the chat UI stuck on "loading" against a dead stream. On
      // web this also covers the (less reliable) resume path; the primary
      // web recovery is the visibilitychange listener above.
      ChatService().handleAppResumed();
    }
  }

  Future<void> _initDeepLinks() async {
    final links = AppLinks();
    try {
      final initial = await links.getInitialLink();
      if (initial != null) _handleUri(initial);
    } catch (_) {}
    _linkSub = links.uriLinkStream.listen(_handleUri, onError: (_) {});
  }

  void _handleUri(Uri uri) {
    final id = parseConversationDeepLink(uri);
    if (id == null) return;
    ref.read(pendingConversationIdProvider.notifier).state = id;
  }

  @override
  void dispose() {
    WidgetsBinding.instance.removeObserver(this);
    _linkSub?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final isMobile = MediaQuery.of(context).size.width < 820;
    final preferences = ref.watch(preferencesProvider);
    final darkModeValue = preferences.darkMode;
    final isDesktop =
        !kIsWeb && (Platform.isWindows || Platform.isLinux || Platform.isMacOS);
    final defaultZoom = isDesktop ? 1.1 : 1.0;
    final zoomFactor =
        preferences.zoomFactor < 0 ? defaultZoom : preferences.zoomFactor;

    ThemeMode themeMode;
    switch (darkModeValue) {
      case 0:
        themeMode = ThemeMode.system;
        break;
      case 1:
        themeMode = ThemeMode.light;
        break;
      case 2:
        themeMode = ThemeMode.dark;
        break;
      default:
        themeMode = ThemeMode.system;
    }

    final authState = ref.watch(authStateProvider);

    return MaterialApp(
      title: 'Plurality',
      theme: lightTheme,
      darkTheme: darkTheme,
      themeMode: themeMode,
      builder: (context, child) {
        return MediaQuery(
          data: MediaQuery.of(context).copyWith(
            textScaler: TextScaler.linear(zoomFactor),
          ),
          child: child!,
        );
      },
      home: authState.when(
        data: (user) {
          if (user == null) {
            // Logged out (or token rejected) — tear down any live SSE/status
            // streams so their retry loops stop hammering the server with a
            // stale token while we sit on the login screen.
            ChatService().disconnect();
            return LoginScreen();
          }
          ChatService().ensureConnected();
          return ChatScreen(isMobile: isMobile);
        },
        loading: () => LoadingScreen(),
        error: (_, __) => ErrorScreen(),
      ),
      routes: {
        '/login': (context) => LoginScreen(),
        '/account': (context) => SettingsScreen(),
      },
    );
  }
}

class LoadingScreen extends StatelessWidget {
  @override
  Widget build(BuildContext context) {
    return Scaffold(body: Center(child: CircularProgressIndicator()));
  }
}

class ErrorScreen extends StatelessWidget {
  @override
  Widget build(BuildContext context) {
    return Scaffold(
      body: Center(
        child: Column(
          mainAxisAlignment: MainAxisAlignment.center,
          children: [
            Icon(Icons.error_outline, color: Colors.red, size: 48),
            SizedBox(height: 16),
            Text('An error occurred. Please try again later.'),
            SizedBox(height: 16),
            ElevatedButton(
              onPressed: () {
                AuthService().signOut();
              },
              child: Text('Sign Out'),
            ),
          ],
        ),
      ),
    );
  }
}
