export const LAUNCHER_SEEN_PATH = '/__clank/launcher/seen';

export const shouldShowLauncherCoachmark = (hasSeenLauncher) => !hasSeenLauncher;

export const launcherShortcut = (isMac) => isMac ? '⌘E' : 'Ctrl E';

export const launcherMorphGeometry = (launcherRect, boxRect) => ({
  x: launcherRect.left + launcherRect.width / 2 - (boxRect.left + boxRect.width / 2),
  y: launcherRect.top + launcherRect.height / 2 - (boxRect.top + boxRect.height / 2),
  scaleX: launcherRect.width / boxRect.width,
  scaleY: launcherRect.height / boxRect.height,
});

export const launcherActivity = (agent, aborting) => {
  if (aborting) return { state: 'stopping', label: 'Clank is stopping', isBusy: true };
  switch (agent) {
    case 'thinking': return { state: 'thinking', label: 'Clank is thinking', isBusy: true };
    case 'working': return { state: 'working', label: 'Clank is working', isBusy: true };
    case 'done': return { state: 'done', label: 'Clank finished', isBusy: false };
    case 'error': return { state: 'error', label: 'Clank needs attention', isBusy: false };
    case 'idle': return { state: 'idle', label: 'Open Clank', isBusy: false };
    default: return { state: 'error', label: 'Clank needs attention', isBusy: false };
  }
};
