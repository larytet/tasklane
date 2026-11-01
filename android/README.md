# Tasklane for Android

A native (Kotlin, Jetpack Compose, Material 3) read-mostly client for a Tasklane server. It talks to the server's own REST
API under `/api/v1`; nothing on the server had to change.

**What it does:** sign in (password, and an authenticator-app code when the account needs one), browse projects, search and
page through issues, open an issue (fields, description, comments), read a board column by column, read the notification
inbox, and add comments. **What it does not do yet:** edit fields, move issues, create issues, passkey sign-in, push.

**The lens:** the issue list is a focus-and-context view. About two thirds of the screen shows a few consecutive issues in full
(highlighted); every other issue is squeezed above and below it, thinner the farther away (a single line, then a coloured
stripe per status). Touch or drag outside the highlight to slide it there, tap inside it to open that issue. It loads up to
200 issues for the view, adapts to the screen height (a taller or higher-resolution screen gets a wider window, not taller
rows), and the magnifier button next to the search box switches back to the plain scrolling list. The layout rule is a pure
function ([`Lens.kt`](app/src/main/java/org/tasklane/android/ui/Lens.kt)) with unit tests; the
[interactive mock](https://larytet.github.io/tasklane/android/) uses the same rule.

It follows the device's light or dark setting (screenshots are in the [main README](../README.md#android)).

**Sign-in:** the server returns the session token as the `tl_session` cookie; the app keeps that value and sends it as
`Authorization: Bearer`. Sign out also sends it as the cookie, because that is the only form the server's logout revokes.
The token lives in app-private storage (`allowBackup` is off). The server address may be plain `http` for a LAN install.

## Install on a phone

Download `tasklane-android-<version>.apk` from the [Releases](https://github.com/larytet/tasklane/releases) page and open it
on the phone (Android 8.0 or newer). Android asks once to allow installs from your browser or file manager. Or from a computer:
`adb install tasklane-android-0.2.0.apk`. Check the download against the `.sha256` file next to it. Updates install over the
old version as long as they are signed with the same key. The app asks for the server address, so a self-hosted install
works as it is, over `https` or plain `http` on your network.

## Build

Needs JDK 17+ and the Android SDK (platform 35, build-tools 35). Point `local.properties` at it (`sdk.dir=...`).

    ./gradlew :app:assembleDebug          # app/build/outputs/apk/debug/app-debug.apk
    ./gradlew :app:assembleRelease        # signed if ~/Android/tasklane-release.properties (or $TASKLANE_SIGNING) exists
    ./gradlew :app:testDebugUnitTest      # 13 mock-server tests and 7 lens-layout tests

`LiveServerTest` runs the real client against a real server and is skipped unless `TASKLANE_IT_URL`, `TASKLANE_IT_EMAIL`
and `TASKLANE_IT_PASSWORD` are set. Use a throwaway server: it needs a project `SHOP` with issue `SHOP-1` ("Checkout fails on 3DS challenge") and a board (the data the happy-path script uses), and
it adds a comment.

## Happy-path test on an emulator

`scripts/happy_path.py` drives the installed app over `adb`: sign in, search, open an issue, comment, projects, a board,
the inbox, sign out. It stops at the first screen that is not what it expects. See the header of the script for the data
the server needs; run it once per theme:

    sdkmanager "emulator" "system-images;android-35;google_apis;x86_64"
    avdmanager create avd -n pixel_api35 -k "system-images;android-35;google_apis;x86_64" -d pixel_6
    emulator -avd pixel_api35 &            # needs KVM to be usable
    adb install -r app/build/outputs/apk/debug/app-debug.apk
    python3 scripts/happy_path.py dark     # and: light

The emulator reaches your machine at `10.0.2.2`; the script signs in to `http://10.0.2.2:18091`.

## Signing a release

`app/build.gradle.kts` reads `storeFile`, `storePassword`, `keyAlias` and `keyPassword` from a properties file outside the
repository (`$TASKLANE_SIGNING`, default `~/Android/tasklane-release.properties`). Without it `assembleRelease` makes an
unsigned APK that will not install. Keep the key: an app signed with a different key cannot update an installed one.
