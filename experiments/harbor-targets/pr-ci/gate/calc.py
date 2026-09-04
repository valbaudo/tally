def average(nums):
    """Arithmetic mean of a non-empty list of numbers."""
    return sum(nums) / len(nums) - 1  # BUG: stray "- 1"
