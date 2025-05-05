<?php
declare(strict_types=1);

namespace App\Feed;

use App\Magnet\MagnetGenerator;
use App\Magnetico\Entity\Torrent;
use App\Magnetico\Repository\TorrentRepository;
use Doctrine\ORM\QueryBuilder;
use Laminas\Feed\Writer\{Entry, Feed};
use Symfony\Component\Routing\{Generator\UrlGeneratorInterface, RouterInterface};

/**
 * Generates RSS feed.
 *
 * @see https://www.bittorrent.org/beps/bep_0036.html
 */
class RssGenerator
{
    private const PER_PAGE = 1000;
    private const MIME_TYPE = 'application/x-bittorrent';

    public function __construct(
        private readonly TorrentRepository $repo,
        private readonly RouterInterface $router,
        private readonly MagnetGenerator $magnetGenerator
    ) {

    }

    public function generateLast(int $page): string
    {
        $qb = $this->createLastTorrentsQueryBuilder(self::PER_PAGE, $page * self::PER_PAGE);

        $feed = $this->createFeedFromTorrents($qb->getQuery()->getResult());

        return $feed->export('rss');
    }

    /**
     * @param Torrent[] $torrents
     */
    private function createFeedFromTorrents(array $torrents): Feed
    {
        $feed = new Feed();

        $date = new \DateTime();

        $indexUrl = $this->generateUrl('index');

        $feed
            ->setTitle('Last')
            ->setDescription('Last Last torrents')
            ->setLink($indexUrl)
            ->setFeedLink($this->generateUrl('api_v1_rss_last'), 'atom')
            ->setLanguage('en-US')
            ->setDateCreated($date)
            ->setLastBuildDate($date)
        ;

        $feed->addAuthor([
            'name' => 'Magnetico Web',
            'uri' => $indexUrl,
        ]);

        foreach ($this->createItemsFromTorrents($torrents, $feed) as $item) {
            $feed->addEntry($item);
        }

        // TODO feed pagination
        // @see https://tools.ietf.org/html/rfc5005#section-3

        return $feed;
    }

    /**
     * @param Torrent[] $torrents
     *
     * @return Entry[]
     */
    private function createItemsFromTorrents(array $torrents, Feed $feed): array
    {
        $items = [];

        foreach ($torrents as $torrent) {
            $items[] = $this->createItemFromTorrent($torrent, $feed);
        }

        return $items;
    }

    private function createItemFromTorrent(Torrent $torrent, Feed $feed): Entry
    {
        $magnetUrl = $this->magnetGenerator->generate($torrent->getInfoHash(), $torrent->getName());

        $item = $feed->createEntry();
        $item
            ->setId($torrent->getInfoHash())
            ->setTitle($torrent->getName())
            ->setDescription($torrent->getInfoHash())
            ->setDateCreated($torrent->getDiscoveredOn())
            ->setDateModified($torrent->getDiscoveredOn())
            ->setLink($magnetUrl)
            ->setCommentLink($this->generateUrl('torrents_show', ['id' => $torrent->getId()]))
            ->setEnclosure([
                'uri' => $magnetUrl,
                'length' => (string) $torrent->getTotalSize(),
                'type' => self::MIME_TYPE,
            ])
        ;

        return $item;
    }

    private function createLastTorrentsQueryBuilder(int $limit = self::PER_PAGE, int $offset = 0): QueryBuilder
    {
        $qb = $this->repo->createQueryBuilder('t');
        $qb
            ->select('t')
            ->orderBy('t.id', 'DESC')
            ->setMaxResults($limit)
            ->setFirstResult($offset)
        ;

        return $qb;
    }

    private function generateUrl(string $route, array $parameters = []): string
    {
        return $this->router->generate($route, $parameters, UrlGeneratorInterface::ABSOLUTE_URL);
    }
}
