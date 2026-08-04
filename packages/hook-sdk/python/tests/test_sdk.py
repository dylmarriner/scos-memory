import unittest
from scos_hook.sdk import HookContext, HookResult, HookRegistry


class HookSDKTests(unittest.TestCase):
    def test_dispatch(self):
        reg = HookRegistry()
        reg.register("pre_turn", lambda ctx: HookResult(modified_payload=ctx.payload + " [step1]"))
        reg.register("pre_turn", lambda ctx: HookResult(modified_payload=ctx.payload + " [step2]"))

        ctx = HookContext("sess1", "t1", "pre_turn", "input")
        res = reg.dispatch(ctx)
        self.assertEqual(res.modified_payload, "input [step1] [step2]")


if __name__ == "__main__":
    unittest.main()
