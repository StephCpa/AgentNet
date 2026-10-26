"""The mutation gate must distinguish safety counterexamples from tool failures."""
import unittest

from mutations import classify


class GateTests(unittest.TestCase):
    def test_named_counterexample(self):
        self.assertTrue(classify(13, "Error: Action property TM!EpochFence is violated.", "EpochFence"))
        self.assertTrue(classify(12, "Error: Invariant Inv5 is violated.", "Inv5"))

    def test_fail_closed(self):
        cases = [
            (1, "Error: Parsing failed"),
            (0, "Model checking completed. No error has been found."),
            (12, "Error: Invariant TypeOK is violated."),
            (1, "Error: Invariant Inv5 is violated."),
            (13, "Error: The behavior up to this point is:"),
        ]
        for code, output in cases:
            with self.subTest(code=code, output=output):
                self.assertFalse(classify(code, output, "Inv5"))

    def test_baseline_requires_completion(self):
        self.assertFalse(classify(0, "Starting..."))
        self.assertFalse(classify(1, "Model checking completed. No error has been found."))
        self.assertTrue(classify(0, "Model checking completed. No error has been found."))


if __name__ == "__main__":
    unittest.main()
