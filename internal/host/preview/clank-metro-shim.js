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
  var Module, path;
  try {
    Module = require('module');
    path = require('path');
  } catch (e) {
    return;
  }

  var RUNTIME = process.env.CLANK_PREVIEW_RUNTIME;
  if (!RUNTIME) return;
  var RUNTIME_DIR = path.dirname(RUNTIME);

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
    //     getter, so reassigning there silently no-ops).
    if (
      norm.indexOf('@expo/metro-config') !== -1 &&
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
        try {
          if (args && typeof args === 'object' && isInitializeCore(args.filename)) {
            var plugins = Array.isArray(args.plugins) ? args.plugins.slice() : [];
            plugins.push(appendRequirePlugin);
            args = Object.assign({}, args, { plugins: plugins });
          }
        } catch (e) {}
        return origTransform.call(this, args);
      };
      exported.__clankTxWrapped = true;
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
            } catch (e) {}
          },
        },
      },
    };
  }
})();
