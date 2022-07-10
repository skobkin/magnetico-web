<?php
declare(strict_types=1);

namespace App\Magnetico\Entity;

use App\Magnetico\Repository\TorrentRepository;
use Doctrine\Common\Collections\ArrayCollection;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Component\Serializer\Annotation as Serializer;

#[ORM\Entity(repositoryClass: TorrentRepository::class, readOnly: true)]
#[ORM\Table(schema: 'magneticod', name: 'torrents')]
#[ORM\Index(name: 'discovered_on_index', columns: ['discovered_on'])]
#[ORM\Index(name: 'info_hash_index', columns: ['info_hash'])]
class Torrent
{
    #[Serializer\Groups(['api_v1_search', 'api_v1_show'])]
    #[ORM\Id]
    #[ORM\Column(name: 'id', type: 'integer')]
    private int $id;

    /** @var resource Resource pointing to info-hash BLOB */
    #[Serializer\Groups(['api_v1_search', 'api_v1_show'])]
    #[ORM\Column(name: 'info_hash', type: 'blob', nullable: false)]
    private $infoHash;

    /** Cached value of self::infoHash in HEX string */
    private ?string $infoHashHexCache = null;

    #[Serializer\Groups(['api_v1_search', 'api_v1_show'])]
    #[ORM\Column(name: 'name', type: 'text', nullable: false)]
    private string $name;

    /** Torrent files total size in bytes */
    #[Serializer\Groups(['api_v1_search', 'api_v1_show'])]
    #[ORM\Column(name: 'total_size', type: 'bigint', nullable: false)]
    private int $totalSize;

    /** Torrent discovery timestamp */
    #[Serializer\Groups(['api_v1_search', 'api_v1_show'])]
    #[ORM\Column(name: 'discovered_on', type: 'integer', nullable: false)]
    private $discoveredOn;

    /** @var File[]|ArrayCollection */
    #[Serializer\Groups(['api_v1_show'])]
    #[ORM\OneToMany(targetEntity: File::class, fetch: 'EXTRA_LAZY', mappedBy: 'torrent')]
    private $files;

    public function getId(): int
    {
        return $this->id;
    }

    /** Returns torrent info hash as HEX string */
    public function getInfoHash(): string
    {
        if (null === $this->infoHashHexCache) {
            $this->infoHashHexCache = bin2hex(stream_get_contents($this->infoHash));
            rewind($this->infoHash);
        }

        return $this->infoHashHexCache;
    }

    public function getName(): string
    {
        return $this->name;
    }

    /** Returns torrent files total size in bytes */
    public function getTotalSize(): int
    {
        return $this->totalSize;
    }

    /** Returns torrent discovery timestamp */
    public function getDiscoveredOn(): int
    {
        return $this->discoveredOn;
    }

    /**
     * @return File[]|ArrayCollection
     */
    public function getFiles(): iterable
    {
        return $this->files;
    }
}
