import unittest

from calc import average


class TestAverage(unittest.TestCase):
    def test_average_of_three(self):
        self.assertEqual(average([1, 2, 3]), 2)

    def test_average_of_one(self):
        self.assertEqual(average([5]), 5)


if __name__ == "__main__":
    unittest.main()
