<?php

namespace App\Form;

use App\Form\Data\PasswordResetRequestData;
use Symfony\Component\Form\{AbstractType, Extension\Core\Type\EmailType, FormBuilderInterface};
use Symfony\Component\OptionsResolver\OptionsResolver;

class PasswordResetRequestType extends AbstractType
{
    public function buildForm(FormBuilderInterface $builder, array $options)
    {
        $builder->add('email', EmailType::class);
    }

    public function configureOptions(OptionsResolver $resolver)
    {
        $resolver->setDefaults([
            'data_class' => PasswordResetRequestData::class,
        ]);
    }
}
