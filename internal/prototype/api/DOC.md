# PROTOTYPE — throwaway, not production

Answers one question, from [The API: smallest surface that expresses all four protocols](https://github.com/valbaudo/dawn/issues/26):

> What is the smallest Go surface that expresses CyberGym, MDASH, VDH and the PR protocol,
> with **no construct that isn't forced by at least one of the four**?

Method: the four protocols are drafted **first and independently**, then the surface is derived
from what they actually needed — so every construct is traceable to the protocol that forced it,
rather than invented up front and rationalised afterwards.

Nothing here is production code. It exists to be read, checked, and thrown away; only the
validated decision graduates. Lives on branch `prototype/api-surface`.
