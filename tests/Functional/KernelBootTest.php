<?php

declare(strict_types=1);

namespace App\Tests\Functional;

use Symfony\Bundle\FrameworkBundle\Test\KernelTestCase;

class KernelBootTest extends KernelTestCase
{
    public function testKernelBootsAndContainerCompiles(): void
    {
        self::bootKernel();

        $container = static::getContainer();

        self::assertTrue($container->has('doctrine.orm.default_entity_manager'));
        self::assertTrue($container->has('doctrine.orm.magneticod_entity_manager'));
        self::assertTrue($container->has('twig'));
    }
}
