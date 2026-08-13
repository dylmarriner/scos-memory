# Hermes Agent integration

The `plugins/scos-memory` directory is a native Hermes Agent plugin. Install
it into the user plugin directory with:

```bash
cp -R integrations/hermes/plugins/scos-memory ~/.hermes/plugins/scos-memory
```

It reads the existing `~/.hermes/scos.env` connection settings and registers
the seven SCOS memory tools after Hermes restarts.

The plugin intentionally does not declare `requires_env`: Hermes does not
inherit values from `~/.hermes/scos.env` during plugin discovery, while the
plugin itself safely loads that file at call time.
