# The OpenPak look

> Trimmed copy for this repository; the canonical document lives at
> `Openpak/docs/design.md` and governs. Last synchronised 2026-09-15.

One design across three surfaces: the website, the console link pages, and the phone app. They
are built with completely different tools and must still read as one product, so what follows
is the part that has to stay in step — not a style guide anybody has to admire.

## What we deliberately do not serve (2026-09-08)

A console asks a lot of servers. Answering one badly is worse than not
redirecting it at all, so each is a decision rather than an omission.

…

Served today: device auth (dauth/aauth/dcert/acert), dragons eLicenses, BAAS
users and friends, Nintendo Accounts, Penne push, Vermillion, Beach, capi
memberships, NPLN game servers, BCAT news (empty), OLSC saves (served by `saves` through the adapter; console acceptance of the key seed package is M0, untested).

## The tinted row

The one shape worth learning. Every list of things-with-a-state uses it: name on the left, pill
on the right, and **the tint carries the state so the pill carries the fact**.

```
┌──────────────────────────────────────────────┐
│  Stardew Valley                  ● 2 players │   green  = working
└──────────────────────────────────────────────┘
┌──────────────────────────────────────────────┐
│  Wii U                          ● Being built│   amber  = on its way
└──────────────────────────────────────────────┘
┌──────────────────────────────────────────────┐
│  PSP                              ● Planned  │   grey   = nothing claimed
└──────────────────────────────────────────────┘
```

- **green** — working, online, live.
- **amber** — being built, waiting on you. Most of a roadmap lives here; without it, unfinished
  work has to borrow a colour that means something else.
- **red** — not answering. Keep it for that, or it stops meaning anything.
- **grey** — *nothing is being claimed*: planned work, an offline friend, an expired invitation,
  a server this deployment does not check. Grey is not "slightly wrong".

Because the tint says how a thing is, the pill is free to say **what** it is — a player count,
a title, a time — instead of repeating the colour in words. The pill also carries a dot, so the
state survives for anyone who cannot rely on the tint.

Implementations: `.state-row` (website `input.css`), `.row` (`nx-baas/link.go`), `StateRow`
(`app/lib/widgets.dart`).

## Rules that keep costing us when broken

1. **Mobile first.** The narrow layout is the design; wider screens add to it. The site was
   desktop-first with patches, which is why fixing one page never fixed the next.
2. **No font under 16px on an input.** Below that iOS zooms on focus and leaves the page
   scrolled sideways — a layout bug the reader cannot undo.
3. **44px touch targets**, 60px on a console where a stick is doing the aiming.
4. **Long strings wrap** — friend codes, title ids, tokens. Horizontal scroll is the one thing a
   phone user cannot work around.
5. **No JavaScript for any journey.** The site's menu is a `<details>` element for this reason.
6. **The logo is one file.** `website/web/static/icon.svg`, copied into the app and rasterised
   for launcher icons; a redrawn copy diverges the moment the real one changes, silently.
