# React Native / Expo UX — common mistakes

A **running document** of UX pitfalls that make an Expo app feel broken or
unpolished. Like the performance doc, these are defaults to reach for, not laws —
but most have bitten real apps. The bar: the app should feel native on both iOS
and Android, with nothing clipped, cramped, hidden behind a notch, or unreachable.

## Icons & glyphs — give them room

- **Size the box, not just the glyph.** An icon clipped at its edges almost always
  means the container is smaller than the glyph, or a `lineHeight` shorter than the
  font is squeezing it. Give an icon an explicit square container at least as large
  as the icon, center it (`alignItems` / `justifyContent: 'center'`), and avoid
  `overflow: 'hidden'` on the wrapper unless you mean to clip.
- **Vector icons need a numeric `size`,** not a font-size guess. With
  `@expo/vector-icons`, set `size` and `color` on the icon and have the parent
  reserve `size + padding`. Text-based or emoji icons need `lineHeight` ≥ `fontSize`
  and enough vertical padding, or they crop at the ascender/descender.
- **Don't position emoji or text glyphs by `width`/`height` alone** — they lay out
  on the text baseline; center them inside a fixed box instead.

## Touch targets & hit areas

- **Minimum ~44×44 pt** for anything tappable (Apple HIG; Android ~48 dp). A 16pt
  icon button is a 44pt target *with padding*, not a 16pt one.
- **Use `hitSlop`** to extend the touch area of small controls without changing
  layout — a visually tiny close/back button should still be comfortably tappable.
- **Don't pack tappables edge-to-edge** — adjacent targets that touch cause
  mis-taps. Keep ~8pt between independent actions.

## Safe areas & device chrome

- **Never hardcode top/bottom padding for the notch or home indicator.** Use
  `react-native-safe-area-context` (`useSafeAreaInsets()` or `SafeAreaView`) and
  pad by the real insets — they differ across devices and orientation.
- **Respect both ends.** Content must clear the status bar/notch at the top and the
  home indicator/gesture bar at the bottom. Full-bleed backgrounds may extend under
  them, but interactive and text content should inset.
- **Test on a notched device and a flat one** (and both orientations if supported) —
  a layout that looks right on one clips on the other.

## Keyboard

- **The keyboard must not cover the focused input or the primary action.** Use a
  keyboard-aware approach (`KeyboardAvoidingView` with the correct per-platform
  `behavior`, or `react-native-keyboard-controller`); see the performance doc for
  doing this smoothly.
- **Keep content scrollable while the keyboard is up** so the submit button stays
  reachable, and dismiss the keyboard on scroll / tap-out where it makes sense.

## Layout stability — nothing should jump

- **Reserve space for elements that appear/disappear** (errors, badges, spinners).
  Fill state in place — fade a checkmark in — rather than reflowing the screen.
- **Handle all three states of every async surface:** loading, empty, and error —
  not just the happy path. Prefer skeletons that match the final layout over a
  centered spinner that then shoves content into view.
- **Give images explicit dimensions and a `resizeMode`** so they don't pop the
  layout when they load. Reach for `expo-image` (caching, placeholders) on real
  content.

## Text

- **Bound growth with `numberOfLines` + ellipsis** where space is limited; unbounded
  text overflows or pushes siblings off-screen.
- **Don't globally disable font scaling.** Respect the OS Dynamic Type / font-size
  setting and test at a large setting so nothing clips or truncates badly.
- **Mind contrast and minimum size** — body text ~16pt, with sufficient color
  contrast (aim for WCAG AA) so it stays legible in sunlight.

## Platform differences

- **iOS and Android render differently:** shadows (`shadowColor/Offset/Opacity/
  Radius` on iOS vs `elevation` on Android), ripple vs opacity press feedback
  (`Pressable` with `android_ripple`), default fonts, and back-gesture behavior.
  Check both — don't ship an iOS-only look on Android.
- **Prefer `Pressable`** and give visible pressed feedback so taps feel responsive.

## Accessibility (also just better UX)

- **Label icon-only controls** with `accessibilityLabel` and set `accessibilityRole`.
  Unlabeled icon buttons are invisible to screen readers — and often a sign the
  affordance is unclear to everyone.
- **Mark headings and group related content** so assistive navigation is sane.
