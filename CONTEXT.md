# Pixiegrabber

Pixiegrabber collects visual references from Pixieset client galleries for organized local use.

## Language

**Collection**:
A top-level Pixieset client gallery that contains zero or more sets.
_Avoid_: Gallery

**Set**:
A named group of references within a collection.
_Avoid_: Gallery, album

**Reference**:
A Pixieset gallery image or video preserved locally together with the metadata that identifies and describes its source. Within a collection, its Pixieset media ID defines its identity across renames and set moves.
_Avoid_: Asset, item

**Placement**:
The membership of a reference in a set. Each placement has its own local file path, so one reference can appear in multiple set folders.
_Avoid_: Copy, duplicate
