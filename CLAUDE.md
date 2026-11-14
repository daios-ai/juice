What Juice is
=============

Read this before answering any design question.

- Juice is the kernel of Web 4.0 (the DAIOS vision paper): a web of composable,
  priced services, where micropayments replace advertising and subscriptions.
- Juice is a distributed operating system for that web. Kernels are its machines,
  actions are its programs, and native actions are its system calls. A process is
  one funded run; its owner pays, and a price bounds everything a call can spend.
- Local and remote calls behave identically. Whether a call crosses a kernel never
  changes what a program can see or do; only the cost may differ.
- People read addresses; programs keep principals. A principal means the same on
  every kernel.
- Trust is earned after the fact. Ratings and signed trade evidence are public so
  that anyone can vet a service; who rated, and what was traded, stay private.
- For every design question, find the operating-system precedent first (Unix,
  Windows, NFS, Kerberos, Ethereum), then check requirements.md, which is
  authoritative. If the vision and the letter of the spec conflict, say so; do not
  resolve it silently.

Rules
=====

- Never implement anything that violates requirements.md
- Never implement code if I don't ask you directly
- Keep the number of files small because this codebase is critical and must be correct!
- Every GO file must have a corresponding test file
- Never commit before passing ALL unit tests and user flows!
- Never split work into phases, stages, or separate commits; deliver the full scope in one pass.
- Native actions should be encapsulated, never hardwired into the kernel.
- If you ever touch requirements.md, you MUST FOLLOW THE INSTRUCTIONS FOR CHANGING IT.
- The network simulation (netsim/, make netsim) tests and measures the behaviour of a whole Juice economy on play, anvil and Sepolia. It does not gate a commit. Any change to federation, pricing, settlement, the rail, or recovery must update the simulation to match and be followed by a run on play and anvil before the change is considered done.
- Never add a Claude-Session trailer or any session identifier to commit messages.

Answering questions
===================

In all your answers:

- use academic prose and tone
- be concise and direct
- it is RUDE TO SPAM THE CONVERSATION - always provide answers proportionate to the questions
- never beat around the bush, never use theatricals
- do not introduce vague nomenclature, stick to what's being used
  or to what's standard in the literature
- always provide logical or mathematical justification.

