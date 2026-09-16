"""負荷試験の正しさ判定が、応答不正を見落とさないことを検証する。"""
import copy
import json
import unittest
from unittest.mock import patch

import run


class ValidationTest(unittest.TestCase):
    def setUp(self):
        self.database = {"orders": 2, "success": 2, "shortage": 0, "items": 40,
                         "invalid_products": 0, "invalid_orders": 0, "invalid_items": 0}
        self.summary = {"metrics": {
            "successful_orders": {"values": {"count": 2}},
            "shortage_orders": {"values": {"count": 0}},
            "checks": {"values": {"rate": 1}},
            "http_req_duration": {"thresholds": {"p(95)<=500": {"ok": False}}},
        }}

    def validate(self, summary):
        with patch.object(run, "sql", return_value=json.dumps(self.database)):
            return run.validate({}, summary, 20)["passed"]

    def test_performance_threshold_failure_does_not_invalidate_correct_orders(self):
        self.assertTrue(self.validate(self.summary))

    def test_response_check_failure_or_missing_is_rejected(self):
        for rate in (0, 0.5, None):
            with self.subTest(rate=rate):
                summary = copy.deepcopy(self.summary)
                if rate is None:
                    del summary["metrics"]["checks"]
                else:
                    summary["metrics"]["checks"]["values"]["rate"] = rate
                self.assertFalse(self.validate(summary))

    def test_database_and_client_count_mismatch_is_rejected(self):
        self.database["success"] = 1
        self.assertFalse(self.validate(self.summary))

    def test_stock_inconsistency_is_rejected(self):
        self.database["invalid_products"] = 1
        self.assertFalse(self.validate(self.summary))


if __name__ == "__main__":
    unittest.main()
