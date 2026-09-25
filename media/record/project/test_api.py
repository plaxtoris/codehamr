import unittest

from api import request


class MessageTests(unittest.TestCase):
    def test_valid_message(self):
        self.assertEqual(request({"message": " hello "}), (201, {"message": "hello"}))

    def test_empty_input(self):
        self.assertEqual(request({})[0], 400)

    def test_blank_message(self):
        self.assertEqual(request({"message": "   "})[0], 400)


if __name__ == "__main__":
    unittest.main()
