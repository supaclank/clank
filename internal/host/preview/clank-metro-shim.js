/**
 * clank-metro-shim — preloaded into `expo start` via NODE_OPTIONS=--require.
 *
 * Goal: make clank-preview-runtime.js run inside the guest bundle WITHOUT
 * writing anything into the user's repo. It does that by monkeypatching Metro
 * in-memory, in two places (each runs in a different process — this shim is
 * loaded into the expo CLI AND every Metro worker because they inherit
 * NODE_OPTIONS):
 *
 *  1) The config loader (`@expo/metro-config`'s underlying loadUserConfig) —
 *     in the MAIN expo process: add our runtime's temp dir to watchFolders,
 *     point resolver.nodeModulesPaths at the project's node_modules (so the
 *     external file's `require('react-native')` resolves), and bump
 *     cacheVersion (the in-memory transformer wrap doesn't change the cache
 *     key, so without this Metro can serve a stale InitializeCore transform).
 *
 *  2) The Babel transformer — in each WORKER process: when transforming
 *     react-native/Libraries/Core/InitializeCore.js, append
 *     `require('<runtime>')` to the end. Metro extracts dependencies AFTER
 *     Babel runs, so that injected require makes the runtime a real graph
 *     module (bundled, with a working `require`) that runs right after
 *     InitializeCore — when navigator/console/LogBox/ErrorUtils + native
 *     modules are all ready. (getModulesRunBeforeMainModule only REORDERS
 *     graph modules; it can't add one — that's why the premodule approach
 *     never worked.)
 *
 * MUST never throw: NODE_OPTIONS also applies to `npm install` / npx, and a
 * throwing --require aborts the host process. Everything is wrapped.
 *
 * The runtime's absolute path comes from CLANK_PREVIEW_RUNTIME (set by the
 * backend). No-op if it's unset.
 */
(function installClankMetroShim() {
  var Module, path, fs;
  try {
    Module = require('module');
    path = require('path');
    fs = require('fs');
  } catch (e) {
    return;
  }

  var RUNTIME = process.env.CLANK_PREVIEW_RUNTIME;
  if (!RUNTIME) return;

  // Install exactly once per process (the hook re-ran + nested otherwise).
  if (global.__clankMetroShimInstalled) return;
  global.__clankMetroShimInstalled = true;

  var RUNTIME_DIR = path.dirname(RUNTIME);
  // Log OUTSIDE the watched runtime dir: a growing file inside a watchFolder
  // makes Metro's watcher churn (likely the source of the 500/ConnectException
  // instability). One level up is not watched.
  var LOG_FILE = path.join(path.dirname(RUNTIME_DIR), 'clank-shim.log');

  // Diagnostics → both the expo process stderr AND a file you can just `cat`
  // (<temp>/clank-shim.log). Each line tells us how far the in-memory injection
  // got, independent of the on-device beacon. Cheap; fires only when we patch.
  function log(msg) {
    var line = '[clank-shim] ' + msg + '\n';
    try {
      process.stderr.write(line);
    } catch (e) {}
    try {
      fs.appendFileSync(LOG_FILE, new Date().toISOString() + ' pid' + process.pid + ' ' + line);
    } catch (e) {}
  }
  log('loaded (pid ' + process.pid + ', runtime ' + RUNTIME + ')');

  var origLoad = Module._load;
  Module._load = function (request) {
    var exported = origLoad.apply(this, arguments);
    try {
      maybePatch(request, exported);
    } catch (e) {
      /* never let a patch attempt break a require */
    }
    return exported;
  };

  function maybePatch(request, exported) {
    if (typeof request !== 'string' || !exported) return;
    var norm = request.replace(/\\/g, '/');

    // (1) The Metro config loader — wrap the UNDERLYING module, not the
    //     @expo/metro-config barrel (its re-export is a non-configurable
    //     getter, so reassigning there silently no-ops). The barrel requires it
    //     via a RELATIVE path (`./config/loadUserConfig`), so we must NOT also
    //     require '@expo/metro-config' in the string — match the path segment.
    if (
      norm.indexOf('config/loadUserConfig') !== -1 &&
      typeof exported.loadUserConfig === 'function' &&
      !exported.__clankCfgWrapped
    ) {
      var origLoadCfg = exported.loadUserConfig;
      exported.loadUserConfig = function () {
        var p = origLoadCfg.apply(this, arguments);
        // loadUserConfig is async (returns a Promise).
        if (p && typeof p.then === 'function') {
          return p.then(function (config) {
            try {
              mutateConfig(config);
            } catch (e) {}
            return config;
          });
        }
        try {
          mutateConfig(p);
        } catch (e) {}
        return p;
      };
      exported.__clankCfgWrapped = true;
      log('wrapped loadUserConfig');
    }

    // (2) The Babel transformer — wrap transform() to add our plugin for
    //     InitializeCore only.
    if (
      norm.indexOf('@expo/metro-config') !== -1 &&
      norm.indexOf('babel-transformer') !== -1 &&
      typeof exported.transform === 'function' &&
      !exported.__clankTxWrapped
    ) {
      var origTransform = exported.transform;
      exported.transform = function (args) {
        var self = this;
        var origArgs = args;
        var modified = args;
        var inject = false;
        try {
          if (args && typeof args === 'object' && isInitializeCore(args.filename)) {
            inject = true;
            var plugins = Array.isArray(args.plugins) ? args.plugins.slice() : [];
            plugins.push(appendRequirePlugin);
            modified = Object.assign({}, args, { plugins: plugins });
          }
        } catch (e) {
          inject = false;
        }
        if (!inject) return origTransform.call(self, origArgs);
        // NEVER let our injection break the build (transform is async). On any
        // sync throw OR async reject, fall back to the original un-injected
        // transform so the preview still works — and log why, so we can fix the
        // injection without leaving the user with a 500'd bundle.
        return Promise.resolve()
          .then(function () {
            return origTransform.call(self, modified);
          })
          .catch(function (e) {
            log(
              'InitializeCore transform FAILED — falling back: ' +
                String((e && (e.stack || e.message)) || e),
            );
            return origTransform.call(self, origArgs);
          });
      };
      exported.__clankTxWrapped = true;
      log('wrapped babel transformer');
    }
  }

  function mutateConfig(config) {
    if (!config || typeof config !== 'object') return;

    if (!Array.isArray(config.watchFolders)) config.watchFolders = [];
    if (config.watchFolders.indexOf(RUNTIME_DIR) === -1) {
      config.watchFolders.push(RUNTIME_DIR);
    }

    config.resolver = config.resolver || {};
    if (config.projectRoot) {
      var projNodeModules = path.join(config.projectRoot, 'node_modules');
      var nmp = Array.isArray(config.resolver.nodeModulesPaths)
        ? config.resolver.nodeModulesPaths.slice()
        : [];
      if (nmp.indexOf(projNodeModules) === -1) nmp.push(projNodeModules);
      config.resolver.nodeModulesPaths = nmp;
    }

    // Bust the transform cache once: the in-memory transformer wrap leaves the
    // babel-transformer file bytes unchanged, so the cache key wouldn't move
    // and a previously-cached InitializeCore (without our require) would be
    // served. cacheVersion is a separate key input.
    config.cacheVersion = (config.cacheVersion || '') + '~clankPreview1';
    log(
      'mutated config (projectRoot=' +
        config.projectRoot +
        ', watchFolders+=' +
        RUNTIME_DIR +
        ')',
    );
  }

  function isInitializeCore(filename) {
    if (typeof filename !== 'string') return false;
    return filename
      .replace(/\\/g, '/')
      .endsWith('react-native/Libraries/Core/InitializeCore.js');
  }

  // Babel plugin: append `require('<RUNTIME>')` as the last statement of the
  // file's Program body, in Program.exit so it survives other plugins and
  // lands after InitializeCore's own statements. A bare require() CallExpression
  // is what Metro's collectDependencies (run on the post-Babel AST) recognizes.
  function appendRequirePlugin(babel) {
    var t = babel.types;
    return {
      visitor: {
        Program: {
          exit: function (programPath) {
            try {
              programPath.pushContainer(
                'body',
                t.expressionStatement(
                  t.callExpression(t.identifier('require'), [t.stringLiteral(RUNTIME)]),
                ),
              );
              log('appended require(runtime) to InitializeCore');
            } catch (e) {
              log('FAILED to append to InitializeCore: ' + (e && e.message));
            }
          },
        },
      },
    };
  }
})();
