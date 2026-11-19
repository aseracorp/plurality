"""Tests for the LiteLLM proxy's per-model optimization flags.

Stdlib unittest on purpose: the litellm venv in the container has no pytest,
and this file must be runnable with the interpreter that runs the proxy:

    /app/litellm/litellm_venv/bin/python -m unittest test_litellm_proxy

It guards the toggle path for caveman's stones: the YAML the operator edits ->
the normalized stone dict on /v1/models -> what the Go registry parses. If the
default stone set drifts from what litellm.md promises, per-stone toggling
silently stops working, which is exactly the bug these tests pin down.
"""

import os
import sys
import tempfile
import textwrap
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import litellm_proxy  # noqa: E402


YAML_TEMPLATE = """
model_list:
  - model_name: "probe-model"
    litellm_params:
      model: "openai/probe"
      api_base: "http://127.0.0.1:9/v1"
      api_key: "sk-not-real"
    model_info:
      mode: "chat"
      supports_function_calling: true
{caveman_block}
"""


def write_config(caveman_block):
    """Write a config file and return its path (caller removes it)."""
    fd, path = tempfile.mkstemp(suffix=".yaml", prefix="litellm-test-")
    with os.fdopen(fd, "w") as f:
        f.write(YAML_TEMPLATE.format(caveman_block=textwrap.indent(caveman_block, "      ")))
    return path


def load(caveman_block):
    path = write_config(caveman_block)
    try:
        litellm_proxy.load_config(path)
        return litellm_proxy.model_to_info["probe-model"]
    finally:
        os.unlink(path)


def clear_maps():
    litellm_proxy.model_to_litellm.clear()
    litellm_proxy.model_to_info.clear()
    litellm_proxy.model_to_api_key.clear()


class TestCavemanDefaults(unittest.TestCase):
    def setUp(self):
        clear_maps()

    def test_bare_true_enables_every_documented_stone(self):
        """litellm.md: bare `caveman: true` means "all six stones on".

        This is the regression that made `browse` unenableable in a real
        deployment: the config said `caveman: true`, the docs said all six,
        and the proxy's default set shipped browse: False — so the stone was
        off for every model that opted in the documented way.
        """
        info = load("caveman: true")
        stones = info["caveman"]["stones"]
        self.assertTrue(info["caveman"]["enabled"])
        for stone in ("say_less", "read_less", "browse"):
            self.assertTrue(stones[stone], "bare true must enable %s" % stone)

    def test_account_and_code_mode_stay_off_by_default(self):
        """auto needs a Caveman account, blocks is pre-release: off by default
        is deliberate, and read_less routing must not drag them in."""
        stones = load("caveman: true")["caveman"]["stones"]
        self.assertFalse(stones["auto"])
        self.assertFalse(stones["blocks"])
        self.assertFalse(stones["waste_fixes"])

    def test_explicit_stones_are_honoured_verbatim(self):
        """Per-stone toggling: an explicit dict must not be overwritten by
        defaults, so browse can be turned off (and back on) by the operator."""
        info = load(
            "caveman:\n"
            "  enabled: true\n"
            "  stones:\n"
            "    say_less: false\n"
            "    read_less: true\n"
            "    browse: true\n"
        )
        stones = info["caveman"]["stones"]
        self.assertFalse(stones["say_less"])
        self.assertTrue(stones["read_less"])
        self.assertTrue(stones["browse"])

    def test_missing_stones_block_still_gets_enabled_flag(self):
        info = load("caveman:\n  enabled: true\n")
        self.assertTrue(info["caveman"]["enabled"])

    def test_partial_stones_fill_from_the_shared_default(self):
        """A stones dict naming only some keys must publish the FULL set.

        The Go parser takes an explicit stones block at face value (an absent
        key means off). If the proxy forwarded a partial block verbatim,
        `stones: {say_less: true}` in YAML would silently mean "read_less off",
        and optimizationOwners would hand content ownership to headroom even
        though the operator enabled caveman. Filling from the shared default
        is what keeps partial YAML ergonomic and unambiguous.
        """
        info = load(
            "caveman:\n"
            "  enabled: true\n"
            "  stones:\n"
            "    say_less: true\n"
        )
        stones = info["caveman"]["stones"]
        self.assertTrue(stones["say_less"])
        self.assertTrue(stones["read_less"], "unlisted stone must fall back to the default")
        self.assertTrue(stones["browse"], "unlisted stone must fall back to the default")
        self.assertFalse(stones["auto"], "unlisted stone must fall back to the default")

    def test_explicit_off_survives_the_fill(self):
        """Filling defaults must not steamroller an explicit `false`."""
        info = load(
            "caveman:\n"
            "  enabled: true\n"
            "  stones:\n"
            "    browse: false\n"
            "    read_less: true\n"
        )
        stones = info["caveman"]["stones"]
        self.assertFalse(stones["browse"])
        self.assertTrue(stones["read_less"])

    def test_headroom_never_gets_caveman_stones(self):
        """The normalization loop handles both opt keys; the stones fill-in is
        caveman-only, and headroom's own shape (enabled, keep_last_turns) must
        pass through untouched."""
        info = load("headroom:\n  enabled: true\n  keep_last_turns: 3\n")
        self.assertEqual(info["headroom"], {"enabled": True, "keep_last_turns": 3})

    def test_typo_fails_closed(self):
        """An unusable value disables the proxy rather than enabling it."""
        info = load('caveman: "yes please"')
        self.assertFalse(info["caveman"]["enabled"])


class TestConfigHotReload(unittest.TestCase):
    """The stone toggles only work if editing the YAML reaches the running
    proxy. Before reload_config_if_changed, the Router was built once at
    startup and a config edit needed a container restart."""

    def setUp(self):
        clear_maps()
        litellm_proxy._config_path = None
        litellm_proxy._config_mtime = None
        self._router = litellm_proxy.router

    def tearDown(self):
        litellm_proxy._config_path = None
        litellm_proxy._config_mtime = None
        litellm_proxy.router = self._router
        clear_maps()

    def _start(self, caveman_block):
        path = write_config(caveman_block)
        litellm_proxy._config_path = path
        litellm_proxy.load_config(path)
        litellm_proxy._config_mtime = litellm_proxy.stat_config_mtime()
        return path

    def test_noop_when_config_unchanged(self):
        self._start("caveman: true")
        self.assertFalse(litellm_proxy.reload_config_if_changed())

    def test_edit_is_picked_up(self):
        path = self._start("caveman: true")
        try:
            # Rewrite with browse off. mtime must move: a same-second rewrite
            # would be invisible to an mtime poll, so force it into the future.
            with open(path, "w") as f:
                f.write(YAML_TEMPLATE.format(
                    caveman_block=textwrap.indent(
                        "caveman:\n"
                        "  enabled: true\n"
                        "  stones:\n"
                        "    say_less: true\n"
                        "    read_less: true\n"
                        "    browse: false\n", "      ")))
            st = os.stat(path)
            os.utime(path, (st.st_atime + 5, st.st_mtime + 5))

            self.assertTrue(litellm_proxy.reload_config_if_changed())
            stones = litellm_proxy.model_to_info["probe-model"]["caveman"]["stones"]
            self.assertFalse(stones["browse"], "browse toggle did not reach the live proxy")
            # A second poll must be a no-op (mtime already consumed).
            self.assertFalse(litellm_proxy.reload_config_if_changed())
        finally:
            os.unlink(path)

    def test_removed_model_disappears(self):
        """load_config appends into the module maps, so a reload must clear
        them first or deleted models keep being advertised on /v1/models."""
        path = self._start("caveman: true")
        try:
            with open(path, "w") as f:
                f.write("model_list: []\n")
            st = os.stat(path)
            os.utime(path, (st.st_atime + 5, st.st_mtime + 5))
            self.assertTrue(litellm_proxy.reload_config_if_changed())
            self.assertNotIn("probe-model", litellm_proxy.model_to_info)
        finally:
            os.unlink(path)

    def test_broken_config_keeps_the_last_good_one(self):
        """A typo in the YAML must not take the serving gateway down."""
        path = self._start("caveman: true")
        try:
            before = dict(litellm_proxy.model_to_info)
            with open(path, "w") as f:
                f.write("model_list: [ this is: not: valid: yaml\n")
            st = os.stat(path)
            os.utime(path, (st.st_atime + 5, st.st_mtime + 5))
            self.assertFalse(litellm_proxy.reload_config_if_changed())
            self.assertIn("probe-model", litellm_proxy.model_to_info)
            self.assertEqual(before["probe-model"]["caveman"]["stones"]["browse"],
                             litellm_proxy.model_to_info["probe-model"]["caveman"]["stones"]["browse"])
        finally:
            os.unlink(path)

    def test_missing_file_is_tolerated(self):
        litellm_proxy._config_path = "/nonexistent/litellm_config.yaml"
        litellm_proxy._config_mtime = None
        self.assertFalse(litellm_proxy.reload_config_if_changed())


if __name__ == "__main__":
    unittest.main()
