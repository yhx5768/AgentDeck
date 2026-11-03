#!/usr/bin/env python3
"""agentdeck send 主 Agent 去重门禁的回归测试。"""

import importlib.util
import pathlib
import unittest

SCRIPT = pathlib.Path(__file__).with_name("agentdeck.py")
spec = importlib.util.spec_from_file_location("agentdeck", SCRIPT)
agentdeck = importlib.util.module_from_spec(spec)
spec.loader.exec_module(agentdeck)


class SendGuardTest(unittest.TestCase):
    def test_agent_send_to_main_agent_is_blocked_by_default(self):
        self.assertTrue(agentdeck.main_agent_send_blocked({"main_agent": True}, False))

    def test_allow_flag_bypasses_the_guard(self):
        self.assertFalse(agentdeck.main_agent_send_blocked({"main_agent": True}, True))

    def test_worker_target_is_not_blocked(self):
        self.assertFalse(agentdeck.main_agent_send_blocked({"main_agent": False}, False))
        self.assertFalse(agentdeck.main_agent_send_blocked({}, False))

    def test_source_metadata(self):
        self.assertEqual("agent-send", agentdeck.send_source(False))
        self.assertEqual("agent-send-explicit", agentdeck.send_source(True))


if __name__ == "__main__":
    unittest.main()
