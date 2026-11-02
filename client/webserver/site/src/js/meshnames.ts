/* Random display names (same word lists as client/mesh/names.go). */
const adjectives = [
  'amber', 'bold', 'brave', 'bright', 'brisk', 'calm', 'clever', 'cool', 'crisp', 'daring',
  'eager', 'fair', 'fast', 'fierce', 'gentle', 'glad', 'golden', 'grand', 'happy', 'hardy',
  'keen', 'kind', 'lively', 'lucky', 'mellow', 'merry', 'mighty', 'misty', 'noble', 'patient',
  'plucky', 'proud', 'quick', 'quiet', 'rapid', 'rustic', 'sharp', 'shy', 'silent', 'silver',
  'sly', 'smooth', 'solid', 'steady', 'stout', 'sunny', 'swift', 'tidy', 'true', 'vivid',
  'warm', 'wild', 'wise', 'witty', 'young', 'zesty'
]
const animals = [
  'antelope', 'badger', 'beaver', 'bison', 'bobcat', 'buffalo', 'camel', 'caribou', 'cheetah', 'condor',
  'coyote', 'crane', 'dingo', 'dolphin', 'eagle', 'elk', 'falcon', 'ferret', 'finch', 'fox',
  'gazelle', 'gecko', 'heron', 'hawk', 'ibex', 'jackal', 'jaguar', 'kestrel', 'koala', 'lemur',
  'lynx', 'marmot', 'marten', 'moose', 'narwhal', 'ocelot', 'orca', 'osprey', 'otter', 'owl',
  'panda', 'panther', 'pelican', 'puffin', 'puma', 'raven', 'seal', 'stoat', 'swan', 'tapir',
  'tiger', 'walrus', 'weasel', 'wolf', 'wombat', 'yak', 'zebra'
]

function rnd (n: number): number {
  const a = new Uint32Array(1)
  window.crypto.getRandomValues(a)
  return a[0] % n
}

/* randomName returns an adjective-animal-NNNN name. */
export function randomName (): string {
  return `${adjectives[rnd(adjectives.length)]}-${animals[rnd(animals.length)]}-${String(rnd(10000)).padStart(4, '0')}`
}
